package gitrelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// AdmitSource enforces the source restrictions from GRASP's Git sync model.
// Only HTTPS sources are accepted; localhost, private IPs, credentials,
// query/fragment data, and this relay itself are rejected.
func (g *GitRelay) AdmitSource(raw string) (string, error) {
	return g.admitSource(context.Background(), raw, false)
}

func (g *GitRelay) admitSource(ctx context.Context, raw string, resolve bool) (string, error) {
	u, err := url.Parse(raw)
	privatePeer := g.privatePeer(raw)
	if err != nil || (u.Scheme != "https" && !(privatePeer && u.Scheme == "http")) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid: Git source must be a credential-free HTTPS URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" {
		return "", errors.New("blocked: Git source host is local or address-like")
	}
	if ip := net.ParseIP(host); ip != nil && !g.allowPrivate {
		return "", errors.New("blocked: Git source host is an IP literal")
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return "", errors.New("blocked: Git source host is private")
	}
	if resolve {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return "", fmt.Errorf("Git source DNS lookup: %w", err)
		}
		for _, ip := range ips {
			if privateIP(ip) && !g.allowPrivate {
				return "", errors.New("blocked: Git source resolves to a private address")
			}
		}
	}
	if g.serviceURL != "" && strings.TrimRight(raw, "/") == g.serviceURL {
		return "", errors.New("blocked: Git source points to this relay")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (g *GitRelay) resolvedSource(ctx context.Context, raw string) (string, string, error) {
	source, err := g.admitSource(ctx, raw, false)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", "", err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return "", "", fmt.Errorf("Git source DNS lookup: %w", err)
	}
	for _, ip := range ips {
		if !privateIP(ip) || g.allowPrivate {
			return source, ip.String(), nil
		}
	}
	return "", "", errors.New("blocked: Git source resolves to a private address")
}

func privateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return ip.IsUnspecified()
}

// FetchMissing downloads only the expected refs from an admitted source. Git
// verifies pack checksums and object connectivity; refs are installed only
// after every expected object exists locally.
func (g *GitRelay) FetchMissing(ctx context.Context, r Repository, sources []string, expected map[string]string) error {
	return g.fetchMissing(ctx, r, sources, expected, 0, 0)
}

// fetchMissing is shared by ordinary branch repair and the more restrictive
// PR repair path. A non-zero depth keeps an untrusted PR clone from making the
// hosted repository walk arbitrary history. maxBytes bounds transfer traffic
// across all sources and the isolated destination after each fetch. These
// limits apply only to isolated repositories, never the hosted branch repo.
func (g *GitRelay) fetchMissing(ctx context.Context, r Repository, sources []string, expected map[string]string, depth int, maxBytes int64) error {
	if err := g.ensureRepo(r); err != nil {
		return err
	}
	local := r
	local.Refs = expected
	if g.stateObjectsPresent(ctx, local) {
		return g.installFetchedRefs(r, expected)
	}
	missing := make(map[string]struct{}, len(expected))
	var budget *byteBudget
	if maxBytes > 0 {
		budget = &byteBudget{}
	}
	var lastErr error
	for ref, oid := range expected {
		if oid != "" {
			missing[ref] = struct{}{}
		}
	}
	for _, raw := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget != nil && len(missing) == 0 {
			break
		}
		if budget != nil && budget.used.Load() >= maxBytes {
			return errRepairByteLimit
		}
		// Private object IDs must never be requested from a public source.
		private := r.Private || (g.policy != nil && g.policy().Reads == "members")
		if private && !g.privatePeer(raw) {
			lastErr = errors.New("blocked: private Git source is not a configured peer")
			continue
		}
		source, ip, err := g.resolvedSource(ctx, raw)
		if err != nil {
			lastErr = err
			continue
		}
		var proxy *privateProxy
		if peerBase := g.privatePeerBase(source); peerBase != "" {
			if err := probePrivatePeer(ctx, peerBase, g.allowPrivate); err != nil {
				continue
			}
			g.mu.RLock()
			signer := g.httpAuth
			g.mu.RUnlock()
			proxy, err = newPrivateProxy(ctx, source, signer, g.allowPrivate)
			if err != nil {
				continue
			}
			defer proxy.Close(context.Background())
		}
		fetchSource := source
		if proxy != nil {
			fetchSource = proxy.URL()
		}
		var budgetProxy *repairProxy
		if budget != nil {
			targetIP := ip
			if proxy != nil {
				if parsed, parseErr := url.Parse(proxy.URL()); parseErr == nil {
					targetIP = parsed.Hostname()
				}
			}
			parsedSource, parseErr := url.Parse(fetchSource)
			if parseErr != nil {
				lastErr = parseErr
				continue
			}
			targetHost := parsedSource.Hostname()
			targetPort := parsedSource.Port()
			if targetPort == "" {
				targetPort = "443"
				if parsedSource.Scheme == "http" {
					targetPort = "80"
				}
			}
			budgetProxy, err = newRepairProxy(ctx, targetIP, targetHost, targetPort, budget, maxBytes)
			if err != nil {
				lastErr = err
				continue
			}
			defer budgetProxy.Close()
		}
		for ref, oid := range expected {
			if err := ctx.Err(); err != nil {
				return err
			}
			if budget != nil && budget.used.Load() >= maxBytes {
				return errRepairByteLimit
			}
			if _, needed := missing[ref]; budget != nil && !needed {
				continue
			}
			if oid == "" {
				delete(missing, ref)
				continue
			}
			port := uPort(source)
			resolve := []string{"-c", "http.curloptResolve=" + urlHost(source) + ":" + port + ":" + ip}
			if proxy != nil {
				fetchSource = proxy.URL()
				resolve = nil
			}
			if budgetProxy != nil {
				resolve = nil
			}
			fetchRef := ref
			if strings.HasPrefix(ref, "refs/nostr/") {
				// NIP-34 clone URLs may point to ordinary Git hosts. The
				// signed commit is the source; refs/nostr is our local name.
				if !isObjectID(oid) {
					return errors.New("invalid: PR commit ID")
				}
				fetchRef = oid
			}
			proxySetting := ""
			if budgetProxy != nil {
				proxySetting = budgetProxy.URL()
			}
			args := []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "http.proxy=" + proxySetting, "--git-dir", g.repoPath(r), "fetch", "--no-tags"}
			if depth > 0 {
				args = append(args, "--depth", strconv.Itoa(depth))
			}
			args = append(args, fetchSource, "+"+fetchRef+":"+"refs/tinyrelay/source/"+safeRef(ref))
			// Keep the resolve option before the subcommand; Git accepts config
			// options only before the command name.
			if len(resolve) > 0 {
				args = append([]string{"-c", "http.curloptResolve=" + urlHost(source) + ":" + port + ":" + ip}, args...)
			}
			cmd := exec.CommandContext(ctx, "git", args...)
			if depth > 0 && maxBytes > 0 {
				// Git receives pack data in a child process and does not expose a
				// portable receive-byte limit. Apply the shell's file-size limit
				// to the isolated fetch process so an oversized pack is stopped
				// while it is being written. The unit is 512 bytes in dash and
				// 1024 in bash, so this is a coarse guard; the proxy budget and
				// the cumulative size check below enforce the exact bound.
				limitBlocks := (maxBytes + 511) / 512
				limited := []string{"ulimit -f \"$1\"; shift; exec \"$@\"", "gitfetch", strconv.FormatInt(limitBlocks, 10), "git"}
				limited = append(limited, args...)
				cmd = exec.CommandContext(ctx, "sh", append([]string{"-c"}, limited...)...)
			}
			cmd.Env = append(os.Environ(), "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=", "NO_PROXY=", "no_proxy=", "GIT_CONFIG_NOSYSTEM=1")
			if budget != nil {
				// A URL-specific proxy or rewrite in the user's Git config must
				// not bypass the bounded transport selected for this repair.
				cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+os.DevNull)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("fetch %s: %w: %s", fetchSource, err, strings.TrimSpace(string(out)))
				continue
			}
			if maxBytes > 0 {
				size, err := gitDirSize(g.repoPath(r))
				if err != nil {
					lastErr = fmt.Errorf("inspect isolated Git repository: %w", err)
					continue
				}
				if size > maxBytes {
					return fmt.Errorf("Git source exceeded isolated repair budget: %d bytes", size)
				}
			}
			if !g.objectsPresent(ctx, r, []string{oid}) {
				continue
			}
			delete(missing, ref)
		}
	}
	if len(missing) != 0 {
		if lastErr != nil {
			return fmt.Errorf("Git sources did not provide every expected object: %w", lastErr)
		}
		return errors.New("Git sources did not provide every expected object")
	}
	return g.installFetchedRefs(r, expected)
}

func (g *GitRelay) installFetchedRefs(r Repository, expected map[string]string) error {
	native, err := g.nativeRefs(r)
	if err != nil {
		return err
	}
	for ref, oid := range expected {
		if oid != "" && native[ref] != oid {
			if err := g.updateRef(r, ref, oid); err != nil {
				return err
			}
		}
	}
	return nil
}

func urlHost(raw string) string {
	u, _ := url.Parse(raw)
	h := u.Hostname()
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}
func uPort(raw string) string {
	u, _ := url.Parse(raw)
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

func safeRef(ref string) string {
	return strings.NewReplacer("/", "_", "\\", "_").Replace(strings.TrimPrefix(ref, "refs/"))
}

func gitDirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (g *GitRelay) updateRef(r Repository, ref, oid string) error {
	cmd := exec.Command("git", "--git-dir", g.repoPath(r), "update-ref", ref, oid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("update ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
