package pin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// A release's asset on GitHub has a digest GitHub computed when it was uploaded (the REST API's
// "digest": "sha256:<hex>", for assets uploaded since mid-2025). mamela pin compares it with what
// it downloaded, so that a download changed on the way, or by a mirror, is not pinned.

// maxGitHubReply bounds an answer of GitHub's API.
const maxGitHubReply = 8 << 20

// maxAssetPages bounds the pages of a release's assets read (100 each).
const maxAssetPages = 10

// githubAsset splits a release download URL, https://<host>/<owner>/<repo>/releases/download/<tag>/<asset>.
func (p *Pinner) githubAsset(raw string) (owner, repo, tag, asset string, ok bool) {
	host := p.GitHubHost
	if host == "" {
		host = "github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != host || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", "", "", false
	}
	segs := strings.Split(u.EscapedPath(), "/")
	if len(segs) != 7 || segs[0] != "" || segs[3] != "releases" || segs[4] != "download" {
		return "", "", "", "", false
	}
	var parts [4]string
	for i, s := range []string{segs[1], segs[2], segs[5], segs[6]} {
		v, err := url.PathUnescape(s)
		if err != nil || v == "" {
			return "", "", "", "", false
		}
		parts[i] = v
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

type githubAssetReply struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"`
}

type githubRelease struct {
	ID     int64              `json:"id"`
	Assets []githubAssetReply `json:"assets"`
}

// checkGitHub compares sum, the sha256 of the download of u, with the digest GitHub lists for the
// asset when u is a GitHub release's asset. note says what was compared, for the person.
func (p *Pinner) checkGitHub(ctx context.Context, c *http.Client, u, sum string) (note string, err error) {
	owner, repo, tag, asset, ok := p.githubAsset(u)
	if !ok {
		return "", nil
	}
	api := strings.TrimSuffix(p.GitHubAPI, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	base := api + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases"
	var rel githubRelease
	if err := p.getJSON(ctx, c, base+"/tags/"+url.PathEscape(tag), &rel); err != nil {
		return "", &Error{Code: CodeGitHubUnavailable,
			Message: fmt.Sprintf("GitHub's API did not say what the asset %s of the release %s of %s/%s is: %v", asset, tag, owner, repo, err),
			Hint:    "GitHub answers 60 requests an hour to a machine without a token: wait and run mamela pin again; check that the release exists"}
	}
	found := find(rel.Assets, asset)
	for page := 1; found == nil && len(rel.Assets) >= 100*page && page < maxAssetPages; page++ {
		var more []githubAssetReply
		if err := p.getJSON(ctx, c, fmt.Sprintf("%s/%d/assets?per_page=100&page=%d", base, rel.ID, page+1), &more); err != nil {
			return "", &Error{Code: CodeGitHubUnavailable,
				Message: fmt.Sprintf("GitHub's API did not list the assets of the release %s of %s/%s: %v", tag, owner, repo, err),
				Hint:    "wait and run mamela pin again"}
		}
		rel.Assets = append(rel.Assets, more...)
		found = find(more, asset)
	}
	if found == nil {
		return "", &Error{Code: CodeGitHubMismatch,
			Message: fmt.Sprintf("the release %s of %s/%s on GitHub lists no asset %s, though %s downloaded", tag, owner, repo, asset, u),
			Hint:    "check the URL: a release's download is one of the assets the release lists"}
	}
	if found.Digest == nil || !strings.HasPrefix(*found.Digest, "sha256:") {
		return "; GitHub lists no sha256 for the asset (an asset uploaded before 2025 has none): compare it with the release's checksum file", nil
	}
	if want := strings.TrimPrefix(*found.Digest, "sha256:"); want != sum {
		return "", &Error{Code: CodeGitHubMismatch,
			Message: fmt.Sprintf("the download of %s has sha256:%s, and GitHub lists sha256:%s for the asset %s of the release %s of %s/%s",
				u, sum, want, asset, tag, owner, repo),
			Hint: "do not pin it: the download was changed on the way or by a proxy. Run mamela pin again on another network, and find out why"}
	}
	return "; GitHub's release lists the same sha256", nil
}

func find(assets []githubAssetReply, name string) *githubAssetReply {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

func (p *Pinner) getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "mamela-pin")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", req.URL.Redacted(), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubReply+1))
	if err != nil {
		return err
	}
	if len(data) > maxGitHubReply {
		return fmt.Errorf("%s answered more than %d bytes", req.URL.Redacted(), maxGitHubReply)
	}
	return json.Unmarshal(data, v)
}
