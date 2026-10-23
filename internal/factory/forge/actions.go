// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/go-github/v92/github"

	"github.com/Smana/agent-platform/internal/httpx"
)

// maxArtifactBytes bounds an artifact's zip and the file read out of it.
const maxArtifactBytes = 4 << 20

// WorkflowRuns are the newest completed runs of workflow, a file under .github/workflows, on
// main: one page of 20, newest first. The dead-links probe reads the newest two a schedule or a
// maintainer started. It needs actions: read, which the merger App holds (runbook 12).
func (g *GitHub) WorkflowRuns(ctx context.Context, workflow string) ([]WorkflowRun, error) {
	res, _, err := g.rest.Actions.ListWorkflowRunsByFileName(ctx, g.owner, g.name, workflow, &github.ListWorkflowRunsOptions{
		Branch: "main", Status: "completed", ExcludePullRequests: true, ListOptions: github.ListOptions{PerPage: 20}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list workflow runs", err)
	}
	out := make([]WorkflowRun, 0, len(res.WorkflowRuns))
	for _, r := range res.WorkflowRuns {
		out = append(out, WorkflowRun{ID: r.GetID(), RunAttempt: r.GetRunAttempt(), Event: r.GetEvent(), Branch: r.GetHeadBranch(),
			Actor: r.GetTriggeringActor().GetLogin(), Conclusion: r.GetConclusion(), Created: r.GetCreatedAt().Time})
	}
	slices.SortStableFunc(out, func(a, b WorkflowRun) int { return cmp.Compare(b.Created.UnixNano(), a.Created.UnixNano()) })
	return out, nil
}

// RunArtifact is file inside run runID's unexpired artifact name. GitHub's API answers the
// download with a redirect to its storage (productionresultssa*.blob.core.windows.net); the zip is
// fetched there through the plain egress client, without the installation token, and read whole
// within maxArtifactBytes.
func (g *GitHub) RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error) {
	list, _, err := g.rest.Actions.ListWorkflowRunArtifacts(ctx, g.owner, g.name, runID, &github.ListOptions{PerPage: 100})
	g.mark(err)
	if err != nil {
		return nil, wrap("list run artifacts", err)
	}
	i := slices.IndexFunc(list.Artifacts, func(a *github.Artifact) bool { return a.GetName() == name && !a.GetExpired() })
	if i < 0 {
		return nil, fmt.Errorf("forge: run %d has no artifact %q", runID, name)
	}
	u, _, err := g.rest.Actions.DownloadArtifact(ctx, g.owner, g.name, list.Artifacts[i].GetID(), 0)
	g.mark(err)
	if err != nil {
		return nil, wrap("download an artifact", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q: %w", name, err)
	}
	resp, err := g.plain.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge: fetch artifact %q: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forge: fetch artifact %q: %s", name, resp.Status)
	}
	zipped, err := httpx.ReadBody(resp.Body, maxArtifactBytes)
	if err != nil {
		return nil, fmt.Errorf("forge: fetch artifact %q: %w", name, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q is not a zip: %w", name, err)
	}
	i = slices.IndexFunc(zr.File, func(f *zip.File) bool { return f.Name == file })
	if i < 0 {
		return nil, fmt.Errorf("forge: artifact %q holds no %s", name, file)
	}
	rc, err := zr.File[i].Open()
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	return httpx.ReadBody(rc, maxArtifactBytes)
}

// FileAt is path's content at ref, "" when absent there: the dead-links probe reads a finding's
// file on main.
func (g *GitHub) FileAt(ctx context.Context, path, ref string) (string, error) {
	return g.fileAt(ctx, path, ref)
}
