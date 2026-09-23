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
		query := url.Values{"offset": {strconv.Itoa(offset)}}
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
