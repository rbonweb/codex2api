package proxy

import (
	"net/http"
	"net/url"
	"strings"
)

// GitHub access settings (issue #522): when a deployment's proxy chain reaches the GitHub API, it is often rate limited by a shared IP's
// 60 requests/hour anonymous quota, which breaks CLI version sync, pricing sync, online updates and similar features.
//
//	github_token      adds Authorization to requests to api.github.com, raising the quota to
//	                  5000 requests/hour/token. The token is only sent to api.github.com, never to
//	                  mirrors or other hosts (the pricing sync URL can be changed to any address, so it must not carry the token).
//	github_proxy_url  dedicated outbound proxy for GitHub domains, decoupled from the global proxy: the API goes through a dirty IP + token
//	                  is enough, and high-volume release downloads do not have to use the limited clean link.

// githubAPIHost is the only host allowed to receive the token.
const githubAPIHost = "api.github.com"

// isGithubHost checks whether a host belongs to the official GitHub domains (github.com / api.github.com /
// *.githubusercontent.com).
func isGithubHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return host == "github.com" || host == githubAPIHost ||
		host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

// GithubProxyOrDefault returns the proxy to use for accessing targetURL: if the target is a GitHub domain and a
// dedicated proxy is configured, it uses the dedicated proxy; otherwise it uses the default proxy passed in by the caller (such as the global proxy).
func GithubProxyOrDefault(targetURL, defaultProxy string) string {
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || parsed == nil || !isGithubHost(parsed.Host) {
		return defaultProxy
	}
	if dedicated := strings.TrimSpace(CurrentRuntimeSettings().GithubProxyURL); dedicated != "" {
		return dedicated
	}
	return defaultProxy
}

// ApplyGithubAuth adds Authorization only when the request points to api.github.com and a token is configured.
// Requests that already carry Authorization are not overwritten.
func ApplyGithubAuth(req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	host := strings.ToLower(req.URL.Host)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if host != githubAPIHost || req.Header.Get("Authorization") != "" {
		return
	}
	if token := strings.TrimSpace(CurrentRuntimeSettings().GithubToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
