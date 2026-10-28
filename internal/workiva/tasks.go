package workiva

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Task is a Workiva task on a document section. Approvals are the responses
// of its approval steps, e.g. APPROVE or REJECT.
type Task struct {
	ID, Title, Status, URL string
	Approvals              []TaskApproval
}

// TaskApproval is one reviewer response to a task's approval step.
type TaskApproval struct{ Action, Comment, User, At string }

type taskUser struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type taskJSON struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	SourceURL     string `json:"sourceUrl"`
	ApprovalSteps []struct {
		Responses []struct {
			Action  string `json:"action"`
			Comment string `json:"comment"`
			Created struct {
				DateTime string `json:"dateTime"`
				UserID   string `json:"user_id"`
			} `json:"created"`
		} `json:"responses"`
	} `json:"approvalSteps"`
}

func (t taskJSON) task() Task {
	out := Task{ID: t.ID, Title: t.Title, Status: t.Status, URL: t.SourceURL}
	for _, step := range t.ApprovalSteps {
		for _, r := range step.Responses {
			out.Approvals = append(out.Approvals, TaskApproval{Action: r.Action, Comment: r.Comment, User: r.Created.UserID, At: r.Created.DateTime})
		}
	}
	return out
}

// CreateReviewTask opens a task on a document section, assigned to the
// reviewer, with one approval step the reviewer completes in Workiva.
func (c *Client) CreateReviewTask(ctx context.Context, documentID, sectionID, reviewerID, title, description string, due time.Time) (Task, error) {
	if reviewerID == "" {
		return Task{}, errors.New("review task requires a reviewer")
	}
	user := taskUser{ID: reviewerID, Type: "USER"}
	date := due.UTC().Format("2006-01-02T00:00:00Z")
	body := map[string]any{
		"title": title, "description": description, "assignees": []taskUser{user}, "dueDate": date,
		"approvalSteps": []any{map[string]any{"completionMode": "ONE", "participants": []taskUser{user}, "dueDate": date}},
		"location":      map[string]string{"resource": documentID, "segment": sectionID},
	}
	var out taskJSON
	if _, err := c.documentJSON(ctx, http.MethodPost, "/tasks", body, &out); err != nil {
		return Task{}, err
	}
	if out.ID == "" {
		return Task{}, errors.New("task creation returned no task")
	}
	return out.task(), nil
}

// SectionTasks lists the tasks on one document section. Workiva rejects a
// filter on location.segment, so tasks are filtered by document and then by
// section here.
// ponytail: first 100 tasks of the document; follow @nextLink if documents carry more.
func (c *Client) SectionTasks(ctx context.Context, documentID, sectionID string) ([]Task, error) {
	var page struct {
		Data []struct {
			taskJSON
			Location struct {
				Segment string `json:"segment"`
			} `json:"location"`
		} `json:"data"`
	}
	path := "/tasks?$maxpagesize=100&$filter=" + url.QueryEscape("location.resource eq '"+documentID+"'")
	if _, err := c.documentJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	out := []Task{}
	for _, t := range page.Data {
		if t.Location.Segment == sectionID {
			out = append(out, t.task())
		}
	}
	return out, nil
}

// DeleteTask removes a task; used to clean up after live tests.
func (c *Client) DeleteTask(ctx context.Context, taskID string) error {
	_, err := c.documentJSON(ctx, http.MethodDelete, "/tasks/"+url.PathEscape(taskID), nil, nil)
	return err
}
