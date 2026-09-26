package dispatcher

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"xloom/internal/board"
)

func (s *Scheduler) scheduleInput(ctx context.Context, id string) (board.SchedulePage, error) {
	var input board.SchedulePage
	for offset := 0; ; {
		// Larger compact pages avoid rebuilding the same graph every 100 steps.
		// Older servers ignore limit and keep their existing paging behavior.
		query := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(board.MaxSchedulePageSize)}, "namespace": {s.namespace()}}
		if offset > 0 {
			query.Set("expected_version", input.StateVersion)
		}
		var page board.SchedulePage
		if err := s.Client.Do(ctx, "GET", projectPath(id)+"/scheduling?"+query.Encode(), nil, &page, nil); err != nil {
			return input, err
		}
		if offset == 0 {
			input = page
		} else {
			if page.StateVersion != input.StateVersion || page.Project.Generation != input.Project.Generation {
				return input, errors.New("state_changed: scheduling pages do not match")
			}
			input.Intents = append(input.Intents, page.Intents...)
			input.Steps = append(input.Steps, page.Steps...)
			for key, check := range page.ExecutionChecks {
				if input.ExecutionChecks == nil {
					input.ExecutionChecks = map[string]board.ExecutionCheck{}
				}
				input.ExecutionChecks[key] = check
			}
		}
		if page.NextOffset == 0 {
			return input, nil
		}
		if page.NextOffset <= offset {
			return input, errors.New("scheduling cursor did not advance")
		}
		offset = page.NextOffset
	}
}
