package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func pinIntegrationBase(ctx context.Context, repo string) (string, string, error) {
	branch, err := gitOutput(ctx, repo, "branch", "--show-current")
	if err != nil || strings.TrimSpace(string(branch)) == "" {
		return "", "", errors.New("repository integration branch is unresolved")
	}
	name := strings.TrimSpace(string(branch))
	if strings.HasPrefix(name, "-") {
		return "", "", errors.New("repository integration branch is invalid")
	}
	if _, err := gitOutput(ctx, repo, "check-ref-format", "--branch", name); err != nil {
		return "", "", errors.New("repository integration branch is invalid")
	}
	// Local-only repositories are used by the development API fixtures and have
	// no remote to pin. Real admissions with origin always take the exact remote
	// path below.
	if _, err := gitOutput(ctx, repo, "remote", "get-url", "origin"); err != nil {
		sha, shaErr := gitOutput(ctx, repo, "rev-parse", "--verify", "HEAD^{commit}")
		if shaErr != nil {
			return "", "", fmt.Errorf("resolve local integration HEAD: %w", shaErr)
		}
		return name, strings.TrimSpace(string(sha)), nil
	}
	ref := "refs/remotes/origin/" + name
	if _, err := gitOutput(ctx, repo, "fetch", "--no-tags", "origin", "+refs/heads/"+name+":"+ref); err != nil {
		return "", "", fmt.Errorf("fetch integration branch %q: %w", name, err)
	}
	sha, err := gitOutput(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || !fullCommitID(strings.TrimSpace(string(sha))) {
		return "", "", fmt.Errorf("resolve origin/%s: %w", name, err)
	}
	return name, strings.TrimSpace(string(sha)), nil
}
