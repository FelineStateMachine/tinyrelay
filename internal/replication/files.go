package replication

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// writeArtifact publishes a complete, durable file by same-directory rename.
// Failed generation leaves any prior artifact intact.
func writeArtifact(path string, produce func(io.Writer) error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tiny-artifact-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = produce(file)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func ImportJSONL(ctx context.Context, store *storage.Store, input io.Reader, now int64) error {
	return readJSONLEvents(ctx, input, func(item event.Event) error {
		_, err := store.Save(ctx, item, storage.SaveOptions{Now: now})
		if errors.Is(err, storage.ErrDuplicate) || errors.Is(err, storage.ErrReplaced) {
			return nil
		}
		return err
	})
}

func readJSONLEvents(ctx context.Context, input io.Reader, accept func(event.Event) error) error {
	reader := bufio.NewReader(input)
	for lineNumber := 1; ; lineNumber++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var item event.Event
			if err := json.Unmarshal(line, &item); err != nil {
				return fmt.Errorf("import line %d: %w", lineNumber, err)
			}
			if err := event.Validate(item); err != nil {
				return fmt.Errorf("import line %d: %w", lineNumber, err)
			}
			if err := accept(item); err != nil {
				return fmt.Errorf("import line %d: %w", lineNumber, err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func WriteDump(ctx context.Context, store *storage.Store, output io.Writer, now int64) error {
	rows, err := store.After(ctx, 0, event.Filter{Tags: map[string][]string{}}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := encoder.Encode(row.Event); err != nil {
			return fmt.Errorf("replication: write dump: %w", err)
		}
	}
	return nil
}
