package gitrelay

// This is the delivery-job seam for converting bind.ws git-sqlite/1 backups
// into native bare repositories. Validation happens before any object or ref
// is written; Git remains the final object database and ref transaction.

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

type BackupObject struct {
	OID    string   `json:"oid"`
	Type   string   `json:"type"`
	Size   int      `json:"size"`
	Chunks []string `json:"chunks"`
}

type BackupRepository struct {
	Key         string            `json:"key"`
	Owner       string            `json:"owner"`
	Identifier  string            `json:"identifier"`
	Alternative bool              `json:"alternative"`
	Objects     []BackupObject    `json:"objects"`
	Refs        map[string]string `json:"refs"`
}

type GitBackup struct {
	Format       string             `json:"format"`
	Repositories []BackupRepository `json:"repositories"`
}

func ValidateGitBackup(input []byte) (GitBackup, error) {
	var backup GitBackup
	if err := json.Unmarshal(input, &backup); err != nil {
		return GitBackup{}, fmt.Errorf("invalid Git backup JSON: %w", err)
	}
	if backup.Format != "bind.ws/git-sqlite/1" {
		return GitBackup{}, errors.New("invalid Git backup format")
	}
	seen := make(map[string]struct{}, len(backup.Repositories))
	for i := range backup.Repositories {
		r := &backup.Repositories[i]
		if !isObjectID64(r.Key) || !isPubKey(r.Owner) || !validIdentifier(r.Identifier) {
			return GitBackup{}, errors.New("invalid Git repository identity")
		}
		if _, ok := seen[r.Key]; ok {
			return GitBackup{}, errors.New("duplicate Git repository")
		}
		seen[r.Key] = struct{}{}
		for _, obj := range r.Objects {
			if err := validateBackupObject(obj); err != nil {
				return GitBackup{}, err
			}
		}
		for ref, oid := range r.Refs {
			if !validRef(ref) || !isObjectID(oid) {
				return GitBackup{}, errors.New("invalid Git backup ref")
			}
		}
	}
	return backup, nil
}

func validateBackupObject(obj BackupObject) error {
	if !isObjectID(obj.OID) || obj.Size < 0 || (obj.Type != "blob" && obj.Type != "tree" && obj.Type != "commit" && obj.Type != "tag") || len(obj.Chunks) == 0 {
		return errors.New("invalid Git backup object")
	}
	data, err := decodeBackupObject(obj)
	if err != nil {
		return err
	}
	h := sha1.Sum(append([]byte(fmt.Sprintf("%s %d\x00", obj.Type, len(data))), data...))
	if len(data) != obj.Size || hex.EncodeToString(h[:]) != obj.OID {
		return errors.New("Git backup object hash mismatch")
	}
	return nil
}

func decodeBackupObject(obj BackupObject) ([]byte, error) {
	var compressed bytes.Buffer
	for _, chunk := range obj.Chunks {
		b, err := base64.StdEncoding.DecodeString(chunk)
		if err != nil {
			return nil, errors.New("invalid Git backup chunk")
		}
		compressed.Write(b)
	}
	r, err := zlib.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return nil, errors.New("invalid Git backup compression")
	}
	var data bytes.Buffer
	if _, err := data.ReadFrom(r); err != nil {
		_ = r.Close()
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func isObjectID64(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func isPubKey(s string) bool     { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func validRef(s string) bool {
	if !strings.HasPrefix(s, "refs/") || s == "refs/" || strings.Contains(s, "//") || strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	if strings.ContainsAny(s, "~^:?*[\\") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 127 {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func validIdentifier(id string) bool {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\\"'$`+"`;&|()<>") {
		return false
	}
	for _, r := range id {
		if r <= ' ' {
			return false
		}
	}
	return true
}

// ImportGitBackup installs a validated backup into empty native repositories.
// It refuses pre-existing refs or repositories to avoid silently mixing
// namespaces; callers can stage an import into a fresh Git root and swap it.
func (g *GitRelay) ImportGitBackup(ctx context.Context, input []byte) error {
	backup, err := ValidateGitBackup(input)
	if err != nil {
		return err
	}
	for _, r := range backup.Repositories {
		repo := Repository{Owner: r.Owner, Identifier: r.Identifier, Alternative: r.Alternative, Refs: r.Refs}
		if err := g.ensureRepo(repo); err != nil {
			return err
		}
		for _, ref := range r.Refs {
			if out, err := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(repo), "show-ref", "--verify", ref).CombinedOutput(); err == nil {
				return fmt.Errorf("Git backup target is not empty: %s", strings.TrimSpace(string(out)))
			}
		}
		for _, obj := range r.Objects {
			data, _ := decodeBackupObject(obj)
			cmd := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(repo), "hash-object", "-w", "--stdin", "-t", obj.Type)
			cmd.Stdin = bytes.NewReader(data)
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != obj.OID {
				return fmt.Errorf("install Git object %s: %w", obj.OID, err)
			}
		}
		for ref, oid := range r.Refs {
			if out, err := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(repo), "update-ref", ref, oid).CombinedOutput(); err != nil {
				return fmt.Errorf("install Git ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
			}
		}
		if head := backupHead(r.Refs); head != "" {
			if out, err := exec.CommandContext(ctx, "git", "--git-dir", g.repoPath(repo), "symbolic-ref", "HEAD", head).CombinedOutput(); err != nil {
				return fmt.Errorf("install Git HEAD: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

func backupHead(refs map[string]string) string {
	for _, candidate := range []string{"refs/heads/main", "refs/heads/master"} {
		if _, ok := refs[candidate]; ok {
			return candidate
		}
	}
	branches := make([]string, 0)
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, ref)
		}
	}
	sort.Strings(branches)
	if len(branches) > 0 {
		return branches[0]
	}
	return ""
}
