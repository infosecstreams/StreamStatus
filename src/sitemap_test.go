package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestUpdateSitemapXML(t *testing.T) {
	// Define a sample XML data
	xmlData := []byte(`<urlset>
    <url>
        <priority>0.8</priority>
        <changefreq>monthly</changefreq>
        <loc>http://www.example.com/</loc>
        <lastmod>1988-01-01T00:00:00-07:00</lastmod>
    </url>
    <url>
        <priority>0.5</priority>
        <changefreq>monthly</changefreq>
        <loc>http://www.example.com/page1.html</loc>
        <lastmod>2021-01-01T00:00:00-07:00</lastmod>
    </url>
</urlset>`)

	// Define the expected output XML data
	expectedOutputXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">
    <url>
        <lastmod>` + time.Now().UTC().Format("2006-01-02T15:04:05-07:00") + `</lastmod>
        <loc>http://www.example.com/</loc>
        <changefreq>monthly</changefreq>
        <priority>0.8</priority>
    </url>
    <url>
        <lastmod>2021-01-01T00:00:00-07:00</lastmod>
        <loc>http://www.example.com/page1.html</loc>
        <changefreq>monthly</changefreq>
        <priority>0.5</priority>
    </url>
</urlset>`)

	// Call the updateSitemapXML function
	outputXML, err := updateSitemapXML(xmlData)

	// Check for errors
	if err != nil {
		t.Errorf("updateSitemapXML returned an error: %v", err)
	}

	// Check if the output XML data matches the expected output
	if !reflect.DeepEqual(outputXML, expectedOutputXML) {
		t.Errorf("updateSitemapXML returned unexpected output.\nExpected:\n%s\nGot:\n%s", expectedOutputXML, outputXML)
	}
}

// Test bad XML data
func TestUpdateSitemapXMLBadXML(t *testing.T) {
	// Define a sample XML data
	xmlData := []byte(`urlset
    <url></url>
</urlset`)

	// Call the updateSitemapXML function
	_, err := updateSitemapXML(xmlData)
	fmt.Println(err)
}

// Test no URLs in URL set
func TestUpdateSitemapXMLNoURLs(t *testing.T) {
	// Define a sample XML data
	xmlData := []byte(`<urlset></urlset>`)
	_, err := updateSitemapXML(xmlData)
	if err == nil {
		t.Errorf("updateSitemapXML should have returned an error")
	}
}
