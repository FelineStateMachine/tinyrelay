package gitrelay

import (
	"errors"
	"os"
)

// Native refs can include receive-pack staging work. The signed authority
// file identifies which exact ref/object pairs have actually been published.
func (g *GitRelay) publishedNativeRefs(r Repository) (map[string]string, error) {
	published, err := readRefsFile(g.statePath(r))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	native, err := g.nativeRefs(r)
	if err != nil {
		return nil, err
	}
	for name, oid := range published {
		if native[name] != oid || internalRef(name) {
			delete(published, name)
		}
	}
	return published, nil
}
