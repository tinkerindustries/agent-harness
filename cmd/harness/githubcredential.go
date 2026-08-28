package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/githubauth"
)

// runGitHubCredential implements `harness github-credential`, the git
// credential helper a harness running on a GitHub App points git at
// (internal/githubauth.Sync sets credential.helper to this command;
// docs/GITHUB-APP.md).
//
// Why a helper at all: an App has no standing token. It mints one per
// account it is installed on, each good for an hour, so the credential git
// needs depends on which repository git is talking to and on what time it
// is. A credential *file* can express neither, and neither can an
// environment variable. A helper is git's own answer to exactly this, and
// it is the reason one App can cover a personal account and an organisation
// while a fine-grained personal access token cannot cover both.
//
// The protocol, which git defines: the operation is argv[1], the request is
// key=value lines on stdin terminated by a blank line, and a helper answers
// a `get` by writing the same shape back. Anything this helper cannot
// answer it answers with nothing at all — an empty reply and exit 0 — which
// git takes as "this helper has no credential" and continues with its next
// one, rather than as a failure. That is deliberate for every case below:
// a host that is not github.com, a `store` or `erase` operation (this
// helper has nothing to save and nothing to forget — GitHub is the store),
// and a harness that has not published a callback address because it is on
// the personal-access-token path.
//
// It talks to the harness over HTTP rather than opening the database
// itself. The process holding the App's private key is the one that already
// has the token cache, and a second process minting its own tokens would
// mean a fresh mint on every push. The address and the bearer token come
// from the environment the harness set (internal/githubauth), inherited
// through git.
func runGitHubCredential(ctx context.Context, args []string) error {
	operation := ""
	if len(args) > 0 {
		operation = args[0]
	}
	request, err := readCredentialRequest(os.Stdin)
	if err != nil {
		return err
	}
	if operation != "get" {
		return nil
	}
	if host := request["host"]; host != "" && host != "github.com" {
		return nil
	}
	endpoint := os.Getenv(githubauth.EnvCredentialURL)
	token := os.Getenv(githubauth.EnvCredentialToken)
	if endpoint == "" || token == "" {
		return nil
	}
	owner := credentialOwner(request)
	if owner == "" {
		// Without credential.useHttpPath git sends no path, and there is no
		// owner to mint a token for. Sync sets that option whenever it
		// installs this helper, so reaching here means something else called
		// the helper; say so rather than answering with a token that would be
		// wrong for most repositories.
		return fmt.Errorf("git asked for a github.com credential without a repository path; set credential.useHttpPath=true")
	}
	username, password, err := fetchCredential(ctx, endpoint, token, owner)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "protocol=https\nhost=github.com\nusername=%s\npassword=%s\n", username, password)
	return out.Flush()
}

// readCredentialRequest parses git's key=value request off stdin, stopping
// at the first blank line or at EOF. A value containing "=" is kept whole:
// only the first separator counts.
func readCredentialRequest(r io.Reader) (map[string]string, error) {
	request := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		request[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the credential request from git: %w", err)
	}
	return request, nil
}

// credentialOwner pulls the account name out of git's request. The path is
// what git sends under credential.useHttpPath — "owner/repo.git" for a
// clone URL, "owner/repo" for some other operations — and the owner is its
// first segment.
func credentialOwner(request map[string]string) string {
	path := strings.TrimPrefix(request["path"], "/")
	owner, _, _ := strings.Cut(path, "/")
	return owner
}

// fetchCredential asks the harness for the installation token belonging to
// owner. An error here reaches the operator as git's own failure message
// for the push or clone that triggered it, so it carries the harness's
// wording rather than a status code.
func fetchCredential(ctx context.Context, endpoint, token, owner string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"host": "github.com", "owner": owner})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("asking the harness for a GitHub credential: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("reading the harness's credential reply: %w", err)
	}
	var parsed struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("the harness returned an unparseable credential reply (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error == "" {
			parsed.Error = http.StatusText(resp.StatusCode)
		}
		return "", "", fmt.Errorf("no GitHub credential for %s: %s", owner, parsed.Error)
	}
	if parsed.Password == "" {
		return "", "", fmt.Errorf("the harness returned an empty GitHub credential for %s", owner)
	}
	return parsed.Username, parsed.Password, nil
}
