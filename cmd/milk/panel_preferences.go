package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *model) loadMemoryPreference(path string) {
	m.memoryPreferencePath = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	open, err := strconv.ParseBool(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	m.panelMemory = open
	if m.panelManualOverride == nil {
		m.panelManualOverride = map[panelRegion]bool{}
	}
	m.panelManualOverride[regionMemory] = true
}

func (m *model) saveMemoryPreference() error {
	if m.memoryPreferencePath == "" {
		return nil
	}
	dir := filepath.Dir(m.memoryPreferencePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "memory-panel-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(strconv.FormatBool(m.panelMemory) + "\n")
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), m.memoryPreferencePath)
}
