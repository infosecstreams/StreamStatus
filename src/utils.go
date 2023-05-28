package main

import (
	"log"
	"strings"
)

func contains(arr []string, item string) bool {
	for _, v := range arr {
		if strings.EqualFold(v, item) {
			return true
		}
	}
	return false
}

func containsTags(arr []string, tags []string) bool {
	found := false
	for _, v := range arr {
		for _, tag := range tags {
			if strings.EqualFold(strings.ToLower(v), strings.ToLower(tag)) {
				found = true
				log.Printf("stream tag %s found\n", tag)
			}
		}
	}
	return found
}

func lineIndex(arr []string, item string) int {
	for i, v := range arr {
		if strings.Contains(strings.ToLower(v), strings.ToLower(item)) {
			return i
		}
	}
	return 1
}
