package main

import (
	"fmt"
	"net/url"
	"strings"
)

const defaultRepoURL = "https://github.com/infosecstreams/infosecstreams.github.io"

// repoFromEnv turns SS_GH_REPO into the site repo's clone URL and local
// directory. Empty means the infosecstreams site. Anything else must be an
// https://github.com/<owner>/<repo> URL (a trailing ".git" or "/" is fine).
func repoFromEnv(v string) (repoURL, dir string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultRepoURL, "infosecstreams.github.io", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", "", fmt.Errorf("invalid SS_GH_REPO %q: %s", v, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "https" || u.Host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid SS_GH_REPO %q: want https://github.com/<owner>/<repo>", v)
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if repo == "" || repo == "." || repo == ".." {
		return "", "", fmt.Errorf("invalid SS_GH_REPO %q: want https://github.com/<owner>/<repo>", v)
	}
	// The rest of the bot derives the clone directory from the 5th "/"
	// segment of the URL, so keep it exactly scheme://host/owner/repo.
	return fmt.Sprintf("https://%s/%s/%s", u.Host, owner, repo), repo, nil
}
