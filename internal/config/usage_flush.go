package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FlushUsage keeps the batch queued until it is durably appended. All file
// modifications share usageFileMu; collecting new records only uses usageMu.
func (s *Store) FlushUsage() error {
	maxLines := s.SettingsSnapshot().UsageLogMaxLines
	s.usageFileMu.Lock()
	defer s.usageFileMu.Unlock()
	s.usageMu.Lock()
	batch := append([]map[string]any(nil), s.usageBuf...)
	s.usageMu.Unlock()
	if len(batch) > 0 {
		var raw strings.Builder
		for _, record := range batch {
			line, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode usage: %w", err)
			}
			raw.Write(line)
			raw.WriteByte('\n')
		}
		committed, writeErr := appendUsageBatch(s.path("usage.jsonl"), []byte(raw.String()))
		if !committed {
			return writeErr
		}
		s.usageMu.Lock()
		s.usageBuf = s.usageBuf[len(batch):]
		if len(s.usageBuf) == 0 {
			s.usageBuf = nil
		}
		s.usageMu.Unlock()
		s.usageLineCount.Add(int64(len(batch)))
		if writeErr != nil {
			return writeErr
		}
	}
	if maxLines > 0 && s.usageLineCount.Load() > int64(maxLines) {
		return s.truncateUsageLogLocked(maxLines)
	}
	return nil
}

// On a short write or sync failure, roll back the incomplete batch so retrying
// cannot duplicate its complete prefix or leave an invalid trailing JSON line.
func appendUsageBatch(path string, data []byte) (committed bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	stat, err := f.Stat()
	if err != nil {
		return false, err
	}
	n, err := f.WriteAt(data, stat.Size())
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return false, errors.Join(err, f.Truncate(stat.Size()), f.Sync())
	}
	return true, nil
}

// Called with usageFileMu held. Scan into a fixed-size tail buffer instead of
// reading an arbitrarily large legacy log into memory.
func (s *Store) truncateUsageLogLocked(keep int) error {
	path := s.path("usage.jsonl")
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	lines := make([]string, 0, keep)
	next := 0
	for scan.Scan() {
		if len(lines) < keep {
			lines = append(lines, scan.Text())
		} else {
			lines[next] = scan.Text()
			next = (next + 1) % keep
		}
	}
	err = errors.Join(scan.Err(), in.Close())
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".usage-truncate-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer os.Remove(tmp)
	w := bufio.NewWriter(out)
	for i := 0; i < len(lines); i++ {
		_, err = w.WriteString(lines[(next+i)%len(lines)] + "\n")
		if err != nil {
			break
		}
	}
	err = errors.Join(err, w.Flush(), out.Sync(), out.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	s.usageLineCount.Store(int64(len(lines)))
	return nil
}
