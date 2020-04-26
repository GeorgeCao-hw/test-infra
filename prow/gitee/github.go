package gitee

import (
	"time"

	sdk "gitee.com/openeuler/go-gitee/gitee"
	"k8s.io/test-infra/prow/github"
)

func ConvertGiteePRComment(i sdk.PullRequestComments) github.IssueComment {
	ct, _ := time.Parse(time.RFC3339, i.CreatedAt)
	ut, _ := time.Parse(time.RFC3339, i.UpdatedAt)

	return github.IssueComment{
		ID:        int(i.Id),
		Body:      i.Body,
		User:      github.User{Login: i.User.Login},
		HTMLURL:   i.HtmlUrl,
		CreatedAt: ct,
		UpdatedAt: ut,
	}
}
