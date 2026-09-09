package gitrelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const previewBytes = 256 * 1024

type BrowseRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Path   string `json:"path"`
	View   string `json:"view"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type BrowseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type BrowseCommit struct {
	OID     string   `json:"oid"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Time    int64    `json:"time"`
	Subject string   `json:"subject"`
}

type BrowsePage struct {
	Owner      string   `json:"owner"`
	Identifier string   `json:"identifier"`
	Head       string   `json:"head"`
	Private    bool     `json:"private"`
	Clone      []string `json:"clone"`
	Relays     []string `json:"relays"`
	// Maintainers is filled by the host, which knows about agent grants.
	Maintainers []Maintainer      `json:"maintainers,omitempty"`
	Refs        map[string]string `json:"refs"`
	Commit      string            `json:"commit"`
	Path        string            `json:"path"`
	View        string            `json:"view"`
	Entries     []BrowseEntry     `json:"entries"`
	Content     string            `json:"content"`
	Binary      bool              `json:"binary"`
	Truncated   bool              `json:"truncated"`
	Commits     []BrowseCommit    `json:"commits"`
	Diff        string            `json:"diff"`
	NextOffset  int               `json:"next_offset"`
}

// Repositories returns detached snapshots. Callers must enforce tenant and
// repository read permissions before revealing these records.
func (g *GitRelay) Repositories() []Repository {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows := make([]Repository, 0, len(g.repos))
	for _, r := range g.repos {
		rows = append(rows, cloneBrowseRepo(r))
	}
	sort.Slice(rows, func(i, j int) bool {
		return key(rows[i].Owner, rows[i].Identifier) < key(rows[j].Owner, rows[j].Identifier)
	})
	return rows
}

func cloneBrowseRepo(r Repository) Repository {
	refs := make(map[string]string, len(r.Refs))
	for name, oid := range r.Refs {
		refs[name] = oid
	}
	r.Refs = refs
	r.Clone = append([]string(nil), r.Clone...)
	r.Relays = append([]string(nil), r.Relays...)
	r.Maintainers = append([]string(nil), r.Maintainers...)
	return r
}

func (g *GitRelay) BrowseRepository(owner, identifier string) (Repository, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.repos[key(owner, identifier)]
	if !ok {
		return Repository{}, os.ErrNotExist
	}
	return cloneBrowseRepo(r), nil
}

func validBrowsePath(value string) bool {
	return value == "" || (!strings.ContainsRune(value, 0) && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../"))
}

func newBrowsePage(r Repository, q BrowseRequest) BrowsePage {
	return BrowsePage{Owner: r.Owner, Identifier: r.Identifier, Head: r.Head, Private: r.Private, Clone: r.Clone, Relays: r.Relays, Refs: r.Refs, Path: q.Path, View: q.View, NextOffset: -1}
}

// Browse only reads objects reachable from materialized, signed public refs.
// It never checks out a working tree, invokes a shell, or follows a symlink.
func (g *GitRelay) Browse(ctx context.Context, q BrowseRequest) (BrowsePage, error) {
	if !validBrowsePath(q.Path) || q.Offset < 0 {
		return BrowsePage{}, errors.New("invalid: repository path or offset")
	}
	if q.View == "" {
		q.View = "tree"
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	r, err := g.BrowseRepository(q.Owner, q.Repo)
	if err != nil {
		return BrowsePage{}, err
	}
	page := newBrowsePage(r, q)
	r.Refs, err = g.browseRefs(ctx, r)
	if err != nil {
		return page, err
	}
	page.Refs = r.Refs
	if len(r.Refs) == 0 {
		return page, nil
	}
	page.Commit, err = g.browseCommit(ctx, r, q.Ref)
	if err != nil {
		return page, err
	}
	switch q.View {
	case "tree":
		err = g.browseTree(ctx, r, q, &page)
	case "file":
		err = g.browseFile(ctx, r, q, &page)
	case "history":
		err = g.browseHistory(ctx, r, q, &page)
	case "commit":
		err = g.browseDiff(ctx, r, &page)
	case "activity":
	default:
		err = errors.New("invalid: unknown repository view")
	}
	return page, err
}

func (g *GitRelay) browseCommand(ctx context.Context, r Repository, args ...string) *exec.Cmd {
	base := []string{"--no-pager", "--literal-pathspecs", "-c", "core.quotePath=false", "-c", "log.showSignature=false", "--git-dir", g.repoPath(r)}
	return exec.CommandContext(ctx, "git", append(base, args...)...)
}

func (g *GitRelay) browseRefs(ctx context.Context, r Repository) (map[string]string, error) {
	b, err := g.browseCommand(ctx, r, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/", "refs/nostr/").Output()
	if err != nil {
		return nil, fmt.Errorf("read repository refs: %w", err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && r.Refs[fields[0]] == fields[1] {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, nil
}

func (g *GitRelay) browseCommit(ctx context.Context, r Repository, ref string) (string, error) {
	if ref == "" || ref == "HEAD" {
		ref = strings.TrimPrefix(r.Head, "ref: ")
	}
	for _, candidate := range []string{ref, "refs/heads/" + ref, "refs/tags/" + ref} {
		if oid, ok := r.Refs[candidate]; ok {
			return g.peelBrowseCommit(ctx, r, oid)
		}
	}
	if !hexObject(ref) {
		return "", errors.New("not found: published ref")
	}
	oid, err := g.peelBrowseCommit(ctx, r, ref)
	if err != nil {
		return "", err
	}
	cmd := g.browseCommand(ctx, r, "rev-list", "--max-count=1", "--stdin")
	var input strings.Builder
	input.WriteString(oid + "\n")
	for _, tip := range r.Refs {
		input.WriteString("^" + tip + "\n")
	}
	cmd.Stdin = strings.NewReader(input.String())
	b, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(b)) != 0 {
		return "", errors.New("not found: published commit")
	}
	return oid, nil
}

func hexObject(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func (g *GitRelay) peelBrowseCommit(ctx context.Context, r Repository, oid string) (string, error) {
	if !hexObject(oid) {
		return "", errors.New("invalid: object id")
	}
	b, err := g.browseCommand(ctx, r, "rev-parse", "--verify", "--end-of-options", oid+"^{commit}").Output()
	if err != nil {
		return "", errors.New("not found: commit")
	}
	return strings.TrimSpace(string(b)), nil
}

func readBrowseOutput(cmd *exec.Cmd, limit int64) ([]byte, bool, error) {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err = cmd.Start(); err != nil {
		return nil, false, err
	}
	b, readErr := io.ReadAll(io.LimitReader(out, limit+1))
	truncated := int64(len(b)) > limit
	if truncated || readErr != nil {
		_ = out.Close()
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, false, readErr
	}
	if truncated {
		return b[:limit], true, nil
	}
	if waitErr != nil {
		return nil, false, fmt.Errorf("read Git object: %w", waitErr)
	}
	return b, false, nil
}

func (g *GitRelay) browseTree(ctx context.Context, r Repository, q BrowseRequest, page *BrowsePage) error {
	tree := page.Commit + ":" + q.Path
	cmd := g.browseCommand(ctx, r, "ls-tree", "-z", "-l", tree)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(out)
	readErr := readBrowseTree(reader, q, page)
	if readErr != nil || page.NextOffset >= 0 {
		// Closing this page cancels only its read-only Git child.
		_ = out.Close()
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if page.NextOffset >= 0 {
		return nil
	}
	return waitErr
}

func readBrowseTree(reader *bufio.Reader, q BrowseRequest, page *BrowsePage) error {
	for index := 0; ; index++ {
		row, err := reader.ReadString(0)
		if errors.Is(err, io.EOF) && row == "" {
			return nil
		}
		if err != nil {
			return err
		}
		if index < q.Offset {
			continue
		}
		if len(page.Entries) >= q.Limit {
			page.NextOffset = index
			return nil
		}
		meta, name, ok := strings.Cut(strings.TrimSuffix(row, "\x00"), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return errors.New("invalid Git tree entry")
		}
		var size int64
		if fields[3] != "-" {
			size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return err
			}
		}
		page.Entries = append(page.Entries, BrowseEntry{Name: name, Path: path.Join(q.Path, name), Mode: fields[0], Type: fields[1], OID: fields[2], Size: size})
	}
}

func (g *GitRelay) fileObject(ctx context.Context, r Repository, commit, file string) (string, error) {
	if file == "" || !validBrowsePath(file) {
		return "", errors.New("invalid: file path")
	}
	b, err := g.browseCommand(ctx, r, "rev-parse", "--verify", "--end-of-options", commit+":"+file).Output()
	if err != nil {
		return "", os.ErrNotExist
	}
	oid := strings.TrimSpace(string(b))
	if !hexObject(oid) {
		return "", os.ErrNotExist
	}
	kind, err := g.browseCommand(ctx, r, "cat-file", "-t", oid).Output()
	if err != nil || strings.TrimSpace(string(kind)) != "blob" {
		return "", errors.New("not found: source file")
	}
	return oid, nil
}

func (g *GitRelay) browseFile(ctx context.Context, r Repository, q BrowseRequest, page *BrowsePage) error {
	oid, err := g.fileObject(ctx, r, page.Commit, q.Path)
	if err != nil {
		return err
	}
	b, truncated, err := readBrowseOutput(g.browseCommand(ctx, r, "cat-file", "blob", oid), previewBytes)
	if err != nil {
		return err
	}
	page.Binary = bytes.IndexByte(b, 0) >= 0 || (!utf8.Valid(b) && !truncated)
	page.Truncated = truncated
	if !page.Binary {
		page.Content = strings.ToValidUTF8(string(b), "�")
	}
	return nil
}

func (g *GitRelay) browseHistory(ctx context.Context, r Repository, q BrowseRequest, page *BrowsePage) error {
	args := []string{"log", "--format=%H%x00%P%x00%an%x00%at%x00%s%x00", "--max-count=" + strconv.Itoa(q.Limit+1), "--skip=" + strconv.Itoa(q.Offset), page.Commit, "--"}
	if q.Path != "" {
		args = append(args, q.Path)
	}
	b, truncated, err := readBrowseOutput(g.browseCommand(ctx, r, args...), previewBytes)
	if err != nil {
		return err
	}
	if truncated {
		return errors.New("commit metadata exceeds preview capacity")
	}
	fields := strings.Split(string(b), "\x00")
	for i := 0; i+4 < len(fields); i += 5 {
		if len(page.Commits) >= q.Limit {
			page.NextOffset = q.Offset + q.Limit
			break
		}
		stamp, err := strconv.ParseInt(fields[i+3], 10, 64)
		if err != nil {
			return err
		}
		page.Commits = append(page.Commits, BrowseCommit{OID: strings.TrimSpace(fields[i]), Parents: strings.Fields(fields[i+1]), Author: fields[i+2], Time: stamp, Subject: fields[i+4]})
	}
	return nil
}

func (g *GitRelay) browseDiff(ctx context.Context, r Repository, page *BrowsePage) error {
	if err := g.browseHistory(ctx, r, BrowseRequest{Limit: 1}, page); err != nil {
		return err
	}
	b, truncated, err := readBrowseOutput(g.browseCommand(ctx, r, "show", "--format=", "--no-ext-diff", "--no-textconv", "--no-renames", page.Commit, "--"), previewBytes)
	page.Diff, page.Truncated = strings.ToValidUTF8(string(b), "�"), truncated
	return err
}

// WriteSource streams a published source blob directly to the destination.
func (g *GitRelay) WriteSource(ctx context.Context, q BrowseRequest, dst io.Writer) error {
	r, err := g.BrowseRepository(q.Owner, q.Repo)
	if err != nil {
		return err
	}
	r.Refs, err = g.browseRefs(ctx, r)
	if err != nil {
		return err
	}
	commit, err := g.browseCommit(ctx, r, q.Ref)
	if err != nil {
		return err
	}
	oid, err := g.fileObject(ctx, r, commit, q.Path)
	if err != nil {
		return err
	}
	cmd := g.browseCommand(ctx, r, "cat-file", "blob", oid)
	cmd.Stdout = dst
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stream source: %w", err)
	}
	return nil
}
