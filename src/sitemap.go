package main

import (
	"encoding/xml"
	"fmt"
	"time"
)

type URLSet struct {
	XMLName xml.Name `xml:"urlset"`
	Xmlns   string   `xml:"xmlns,attr"`
	Xhtml   string   `xml:"xmlns:xhtml,attr"`
	URLs    []URL    `xml:"url"`
}

type URL struct {
	LastMod    string `xml:"lastmod"`
	Loc        string `xml:"loc"`
	ChangeFreq string `xml:"changefreq"`
	Priority   string `xml:"priority"`
}

func updateSitemapXML(xmlData []byte) ([]byte, error) {
	// Unmarshal the XML data into URLSet struct
	var urlSet URLSet
	err := xml.Unmarshal([]byte(xmlData), &urlSet)
	if err != nil {
		return nil, err
	}

	// Update the first instance of the date with RFC3339 format
	// This should be the /index.html page data.
	if len(urlSet.URLs) == 0 {
		return nil, fmt.Errorf("no URLs in URLSet")
	}
	newDate := time.Now().UTC().Format("2006-01-02T15:04:05-07:00")
	urlSet.URLs[0].LastMod = newDate

	// Set the urlset XML attributes
	urlSet.Xmlns = "http://www.sitemaps.org/schemas/sitemap/0.9"
	urlSet.Xhtml = "http://www.w3.org/1999/xhtml"

	// Marshal the updated URLSet struct back to XML
	outputXML, _ := xml.MarshalIndent(urlSet, "", "    ")

	// Append the XML header to the output
	outputXML = append([]byte(xml.Header), outputXML...)

	// Return the XML data or an error
	return outputXML, nil
}
