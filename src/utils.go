package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

// contains checks if a given string item is present in a slice of strings arr.
// It returns true if the item is present, false otherwise.
func contains(arr []string, item string) bool {
	for _, v := range arr {
		if strings.EqualFold(v, item) {
			return true
		}
	}
	return false
}

// containsTags checks if any of the tags in the given slice of strings are present in another slice of strings.
// It returns true if any of the tags are present, false otherwise.
// It also logs a message for each tag found in the slice of strings.
func containsTags(arr []string, tags []string) bool {
	found := false
	for _, v := range arr {
		for _, tag := range tags {
			if strings.EqualFold(strings.ToLower(v), strings.ToLower(tag)) {
				found = true
				log.Debugf("stream tag %s found", tag)
			}
		}
	}
	return found
}

// lineIndex returns the index of the first line in a slice of strings that contains the given string item.
func lineIndex(arr []string, item string) (i int) {
	i = -1
	for i, v := range arr {
		if strings.Contains(strings.ToLower(v), strings.ToLower(item)) {
			return i
		}
	}
	return
}

func readCSVLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, ",") {
			return nil, fmt.Errorf("invalid csv line: %s", line)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func writeCSVLines(path string, lines []string) error {
	var builder strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
	return os.WriteFile(path, []byte(builder.String()), 0644)
}

func sortCSVLines(lines []string) {
	sort.SliceStable(lines, func(i, j int) bool {
		return strings.ToLower(csvName(lines[i])) < strings.ToLower(csvName(lines[j]))
	})
}

func csvName(line string) string {
	parts := strings.SplitN(line, ",", 2)
	return strings.TrimSpace(parts[0])
}
