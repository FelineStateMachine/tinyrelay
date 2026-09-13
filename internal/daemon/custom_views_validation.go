package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"image/png"
	"io"
	"strings"
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

const maxArtifactPixels = int64(16 * 1024 * 1024)

func checkArtifact(artifact viewArtifact, maxBytes int) (string, error) {
	switch artifact.Type {
	case "image/svg+xml":
		if len(artifact.Body) > maxBytes {
			return "", errors.New("invalid: artifact exceeds the size limit")
		}
		if err := checkSVG(artifact.Body); err != nil {
			return "", err
		}
		return artifact.Body, nil
	case "image/png":
		body := strings.TrimSpace(artifact.Body)
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(body)
		}
		if err != nil {
			return "", errors.New("invalid: png body must be base64")
		}
		if len(decoded) > maxBytes {
			return "", errors.New("invalid: artifact exceeds the size limit")
		}
		if !bytes.HasPrefix(decoded, pngSignature) {
			return "", errors.New("invalid: png body is not a PNG")
		}
		config, err := png.DecodeConfig(bytes.NewReader(decoded))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return "", errors.New("invalid: png body is not a valid PNG")
		}
		pixels := int64(config.Width) * int64(config.Height)
		if int64(config.Width) > maxArtifactPixels || int64(config.Height) > maxArtifactPixels || pixels > maxArtifactPixels {
			return "", errors.New("invalid: png dimensions exceed the limit")
		}
		if _, err := png.Decode(bytes.NewReader(decoded)); err != nil {
			return "", errors.New("invalid: png body is not a valid PNG")
		}
		return base64.StdEncoding.EncodeToString(decoded), nil
	default:
		return "", errors.New("invalid: artifact type must be image/svg+xml or image/png")
	}
}

func checkSVG(body string) error {
	decoder := xml.NewDecoder(strings.NewReader(body))
	depth, roots, styleDepth := 0, 0, 0
	var style strings.Builder
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("invalid: svg is not well-formed XML")
		}
		switch token := token.(type) {
		case xml.Directive:
			return errors.New("invalid: svg contains a declaration")
		case xml.ProcInst:
			if strings.EqualFold(token.Target, "xml-stylesheet") {
				return errors.New("invalid: svg contains a stylesheet instruction")
			}
		case xml.StartElement:
			if styleDepth > 0 {
				return errors.New("invalid: svg style contains an element")
			}
			name := strings.ToLower(token.Name.Local)
			if depth == 0 {
				roots++
				if name != "svg" {
					return errors.New("invalid: svg body has no svg element")
				}
			}
			if token.Name.Space != "" && token.Name.Space != "http://www.w3.org/2000/svg" {
				return errors.New("invalid: svg has an invalid namespace")
			}
			if name == "script" {
				return errors.New("invalid: svg contains script")
			}
			if name == "foreignobject" {
				return errors.New("invalid: svg contains a foreign object")
			}
			if name == "iframe" || name == "object" || name == "embed" || name == "animate" || name == "set" || name == "animatemotion" || name == "animatetransform" {
				return errors.New("invalid: svg contains active content")
			}
			if name == "style" {
				styleDepth = depth + 1
				style.Reset()
			}
			for _, attr := range token.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				if attrName == "base" && (attr.Name.Space == "xml" || attr.Name.Space == "http://www.w3.org/XML/1998/namespace") {
					return errors.New("invalid: svg has an external base")
				}
				if strings.HasPrefix(attrName, "on") && len(attrName) > 2 {
					return errors.New("invalid: svg contains an event handler")
				}
				value := strings.TrimSpace(attr.Value)
				if attrName == "href" || attrName == "src" {
					if value == "" || !strings.HasPrefix(value, "#") {
						if strings.HasPrefix(strings.ToLower(value), "javascript:") {
							return errors.New("invalid: svg contains a javascript reference")
						}
						return errors.New("invalid: svg references an external target")
					}
				}
				if isCSSAttribute(attrName) && !safeSVGCSSText(value) {
					return errors.New("invalid: svg references an external target")
				}
			}
			depth++
		case xml.EndElement:
			if styleDepth == depth {
				if !safeSVGCSSText(style.String()) {
					return errors.New("invalid: svg references an external target")
				}
				styleDepth = 0
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return errors.New("invalid: svg has content outside its root element")
			}
			if styleDepth > 0 {
				style.Write([]byte(token))
			}
		}
	}
	if roots != 1 || depth != 0 || styleDepth != 0 {
		return errors.New("invalid: svg is not well-formed XML")
	}
	return nil
}

func isCSSAttribute(name string) bool {
	return name == "style" || strings.Contains(name, "fill") || strings.Contains(name, "stroke") || strings.Contains(name, "filter") || strings.Contains(name, "mask") || strings.Contains(name, "clip-path") || strings.Contains(name, "marker-")
}

func safeSVGCSSText(value string) bool {
	clean, ok := stripCSSComments(value)
	if !ok || strings.Contains(clean, "\\") {
		return false
	}
	lower := strings.ToLower(clean)
	compact := strings.ReplaceAll(lower, " ", "")
	return !strings.Contains(lower, "@import") && !strings.Contains(compact, "@import") && localSVGURLs(clean) && localSVGURLs(compact)
}

func stripCSSComments(value string) (string, bool) {
	var out strings.Builder
	for len(value) > 0 {
		start := strings.Index(value, "/*")
		if start < 0 {
			out.WriteString(value)
			return out.String(), true
		}
		out.WriteString(value[:start])
		value = value[start+2:]
		end := strings.Index(value, "*/")
		if end < 0 {
			return "", false
		}
		out.WriteByte(' ')
		value = value[end+2:]
	}
	return out.String(), true
}

func localSVGURLs(value string) bool {
	lower := strings.ToLower(value)
	for {
		at := strings.Index(lower, "url(")
		if at < 0 {
			return true
		}
		end := strings.IndexByte(lower[at+4:], ')')
		if end < 0 {
			return false
		}
		target := strings.Trim(strings.TrimSpace(value[at+4:at+4+end]), "\"'")
		if !strings.HasPrefix(target, "#") {
			return false
		}
		lower = lower[at+4+end+1:]
		value = value[at+4+end+1:]
	}
}
