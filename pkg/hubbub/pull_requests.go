// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hubbub

import (
	"fmt"
	"github.com/google/triage-party/pkg/constants"
	"github.com/google/triage-party/pkg/models"
	"github.com/google/triage-party/pkg/provider"
	"sort"
	"time"

	"github.com/google/go-github/v31/github"
	"github.com/google/triage-party/pkg/tag"
	"k8s.io/klog/v2"
)

const (
	Unreviewed          = "UNREVIEWED"
	NewCommits          = "NEW_COMMITS"
	ChangesRequested    = "CHANGES_REQUESTED"
	Approved            = "APPROVED"
	PushedAfterApproval = "PUSHED_AFTER_APPROVAL"
	Commented           = "COMMENTED"
	Merged              = "MERGED"
	Closed              = "CLOSED"
)

// cachedPRs returns a list of cached PR's if possible
func (h *Engine) cachedPRs(sp models.SearchParams) ([]*models.PullRequest, time.Time, error) {
	sp.SearchKey = prSearchKey(sp)
	if x := h.cache.GetNewerThan(sp.SearchKey, sp.NewerThan); x != nil {
		// Normally the similarity tables are only updated when fresh data is encountered.
		if sp.NewerThan.IsZero() {
			go h.updateSimilarPullRequests(sp.SearchKey, x.PullRequests)
		}
		return x.PullRequests, x.Created, nil
	}

	klog.V(1).Infof("cache miss: %s newer than %s", sp.SearchKey, sp.NewerThan)
	prs, created, err := h.updatePRs(sp)
	if err != nil {
		klog.Warningf("Retrieving stale results for %s due to error: %v", sp.SearchKey, err)
		x := h.cache.GetNewerThan(sp.SearchKey, time.Time{})
		if x != nil {
			return x.PullRequests, x.Created, nil
		}
	}
	return prs, created, err
}

// updatePRs returns and caches live PR's
func (h *Engine) updatePRs(sp models.SearchParams) ([]*models.PullRequest, time.Time, error) {
	start := time.Now()
	sp.PullRequestListOptions = models.PullRequestListOptions{
		ListOptions: models.ListOptions{PerPage: 100},
		State:       sp.State,
		Sort:        constants.UpdatedSortOption,
		Direction:   constants.DescDirectionOption,
	}
	klog.V(1).Infof("%s PR list opts for %s: %+v", sp.State, sp.SearchKey, sp.PullRequestListOptions)

	foundOldest := false
	var allPRs []*models.PullRequest
	for {
		if sp.UpdateAge == 0 {
			klog.Infof("Downloading %s pull requests for %s/%s (page %d)...",
				sp.State, sp.Repo.Organization, sp.Repo.Project, sp.PullRequestListOptions.Page)
		} else {
			klog.Infof("Downloading %s pull requests for %s/%s updated within %s (page %d)...",
				sp.State, sp.Repo.Organization, sp.Repo.Project, sp.UpdateAge, sp.PullRequestListOptions.Page)
		}

		pr := provider.ResolveProviderByHost(sp.Repo.Host)
		prs, resp, err := pr.PullRequestsList(sp)

		if err != nil {
			if _, ok := err.(*github.RateLimitError); ok {
				klog.Errorf("oh snap! We reached the GitHub search API limit: %v", err)
			}
			return prs, start, err
		}
		h.logRate(resp.Rate)

		for _, pr := range prs {
			// Because PR searches do not support opt.Since
			if sp.UpdateAge != 0 {
				if time.Since(pr.GetUpdatedAt()) > sp.UpdateAge {
					foundOldest = true
					break
				}
			}

			h.updateMtime(pr, pr.GetUpdatedAt())

			allPRs = append(allPRs, pr)
		}

		go h.updateSimilarPullRequests(sp.SearchKey, prs)

		if resp.NextPage == 0 || foundOldest {
			break
		}
		sp.PullRequestListOptions.Page = resp.NextPage
	}

	if err := h.cache.Set(sp.SearchKey, &models.Thing{PullRequests: allPRs}); err != nil {
		klog.Errorf("set %q failed: %v", sp.SearchKey, err)
	}

	klog.V(1).Infof("updatePRs %s returning %d PRs", sp.SearchKey, len(allPRs))

	return allPRs, start, nil
}

func (h *Engine) cachedPR(sp models.SearchParams) (*models.PullRequest, time.Time, error) {
	sp.SearchKey = fmt.Sprintf("%s-%s-%d-pr", sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber)

	if x := h.cache.GetNewerThan(sp.SearchKey, sp.NewerThan); x != nil {
		return x.PullRequests[0], x.Created, nil
	}

	klog.V(1).Infof("cache miss for %s newer than %s", sp.SearchKey, sp.NewerThan)
	if !sp.Fetch {
		return nil, time.Time{}, nil
	}

	pr, created, err := h.updatePR(sp)

	if err != nil {
		klog.Warningf("Retrieving stale results for %s due to error: %v", sp.SearchKey, err)
		x := h.cache.GetNewerThan(sp.SearchKey, time.Time{})
		if x != nil {
			return x.PullRequests[0], x.Created, nil
		}
	}
	return pr, created, err
}

// pr gets a single PR (not used very often)
func (h *Engine) updatePR(sp models.SearchParams) (*models.PullRequest, time.Time, error) {
	klog.V(1).Infof("Downloading single PR %s/%s #%d", sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber)
	start := time.Now()

	p := provider.ResolveProviderByHost(sp.Repo.Host)
	pr, resp, err := p.PullRequestsGet(sp)

	if err != nil {
		return pr, start, err
	}

	h.logRate(resp.Rate)
	h.updateMtime(pr, pr.GetUpdatedAt())

	if err := h.cache.Set(sp.SearchKey, &models.Thing{PullRequests: []*models.PullRequest{pr}}); err != nil {
		klog.Errorf("set %q failed: %v", sp.SearchKey, err)
	}

	return pr, start, nil
}

func (h *Engine) cachedReviewComments(sp models.SearchParams) ([]*models.PullRequestComment, time.Time, error) {
	sp.SearchKey = fmt.Sprintf("%s-%s-%d-pr-comments", sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber)

	if x := h.cache.GetNewerThan(sp.SearchKey, sp.NewerThan); x != nil {
		return x.PullRequestComments, x.Created, nil
	}

	if !sp.Fetch {
		return nil, time.Time{}, nil
	}

	klog.V(1).Infof("cache miss for %s newer than %s", sp.SearchKey, sp.NewerThan)
	comments, created, err := h.updateReviewComments(sp)
	if err != nil {
		klog.Warningf("Retrieving stale results for %s due to error: %v", sp.SearchKey, err)
		x := h.cache.GetNewerThan(sp.SearchKey, time.Time{})
		if x != nil {
			return x.PullRequestComments, x.Created, nil
		}
	}
	return comments, created, err
}

// prComments mixes together code review comments and pull-request comments
func (h *Engine) prComments(sp models.SearchParams) ([]*models.Comment, time.Time, error) {
	start := time.Now()

	var comments []*models.Comment
	cs, _, err := h.cachedIssueComments(sp)
	if err != nil {
		klog.Errorf("pr comments: %v", err)
	}
	for _, c := range cs {
		comments = append(comments, models.NewComment(c))
	}

	rc, _, err := h.cachedReviewComments(sp)
	if err != nil {
		klog.Errorf("comments: %v", err)
	}
	for _, c := range rc {
		h.updateMtimeLong(sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber, c.GetUpdatedAt())

		nc := models.NewComment(c)
		nc.ReviewID = c.GetPullRequestReviewID()
		comments = append(comments, nc)
	}

	// Re-sort the mixture of review and issue comments in ascending time order
	sort.Slice(comments, func(i, j int) bool { return comments[j].Created.After(comments[i].Created) })

	if h.debug[sp.IssueNumber] {
		klog.Errorf("debug comments: %s", formatStruct(comments))
	}

	return comments, start, err
}

func (h *Engine) updateReviewComments(sp models.SearchParams) ([]*models.PullRequestComment, time.Time, error) {
	klog.V(1).Infof("Downloading review comments for %s/%s #%d", sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber)
	start := time.Now()

	sp.ListOptions = models.ListOptions{PerPage: 100}
	var allComments []*models.PullRequestComment
	for {
		klog.V(2).Infof("Downloading review comments for %s/%s #%d (page %d)...",
			sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber, sp.ListOptions.Page)

		p := provider.ResolveProviderByHost(sp.Repo.Host)
		cs, resp, err := p.PullRequestsListComments(sp)

		if err != nil {
			return cs, start, err
		}

		h.logRate(resp.Rate)

		klog.V(2).Infof("Received %d review comments", len(cs))
		for _, c := range cs {
			h.updateMtimeLong(sp.Repo.Organization, sp.Repo.Project, sp.IssueNumber, c.GetUpdatedAt())
		}
		allComments = append(allComments, cs...)
		if resp.NextPage == 0 {
			break
		}
		sp.ListOptions.Page = resp.NextPage
	}

	if err := h.cache.Set(sp.SearchKey, &models.Thing{PullRequestComments: allComments}); err != nil {
		klog.Errorf("set %q failed: %v", sp.SearchKey, err)
	}

	return allComments, start, nil
}

func (h *Engine) createPRSummary(sp models.SearchParams, pr *models.PullRequest, cs []*models.Comment,
	timeline []*models.Timeline, reviews []*models.PullRequestReview) *Conversation {
	co := h.createConversation(pr, cs, sp.Age)
	co.Type = PullRequest
	co.ReviewsTotal = len(reviews)
	co.TimelineTotal = len(timeline)
	h.addEvents(sp, co, timeline)

	co.ReviewState = reviewState(pr, timeline, reviews)
	co.Tags = append(co.Tags, reviewStateTag(co.ReviewState))

	if pr.GetDraft() {
		co.Tags = append(co.Tags, tag.Draft)
	}

	// Technically not the same thing, but close enough for me.
	co.ClosedBy = pr.GetMergedBy()
	if pr.GetMerged() {
		co.ReviewState = Merged
		co.Tags = append(co.Tags, tag.Merged)
	}

	sort.Slice(co.Tags, func(i, j int) bool { return co.Tags[i].ID < co.Tags[j].ID })
	return co
}

func (h *Engine) PRSummary(sp models.SearchParams, pr *models.PullRequest, cs []*models.Comment, timeline []*models.Timeline,
	reviews []*models.PullRequestReview) *Conversation {
	key := pr.GetHTMLURL()
	cached, ok := h.seen[key]
	if ok {
		if !cached.Updated.Before(pr.GetUpdatedAt()) && cached.CommentsTotal >= len(cs) && cached.TimelineTotal >= len(timeline) && cached.ReviewsTotal >= len(reviews) {
			return h.seen[key]
		}
		klog.Infof("%s in PR cache, but was invalid. Live @ %s (%d comments), cached @ %s (%d comments)  ", pr.GetHTMLURL(), pr.GetUpdatedAt(), len(cs), cached.Updated, cached.CommentsTotal)
	}

	h.seen[key] = h.createPRSummary(sp, pr, cs, timeline, reviews)
	return h.seen[key]
}
