package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/workspace"
)

// materialiseAttachments writes the images named by ids into an existing
// session's workspace and returns the file names they landed under, in the
// order the ids were given.
//
// The workspace a run starts with gets its attachments from
// internal/workspace.Prepare, before there is a loop at all. This is the
// other half: the images somebody pastes into the composer of a session that
// already has a workspace — a steer into a running session, or a
// continuation of a finished one (docs/RUN-CONTROL.md, "Images in the
// composer"). The bytes travel through the attachments table rather than
// through the event log or the queue request, so the only thing the log and
// the request carry is a list of ids.
//
// It is the loop that writes them, not the HTTP handler that accepted them,
// and the ordering is the reason: the file has to exist before the message
// naming it reaches the model, and the loop is the only place that knows
// when that is. The confinement check lives in
// internal/workspace.WriteAttachments, so a name that could climb out of
// scratch/attachments/ is refused here for the same reason and by the same
// code as one on a fresh workspace.
func (r *Runner) materialiseAttachments(ctx context.Context, ws string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if ws == "" {
		return nil, fmt.Errorf("session: attachments: session has no workspace to write them into")
	}
	atts := make([]workspace.Attachment, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		att, err := r.Store.GetAttachment(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("session: attachments: %s is not in the store", id)
			}
			return nil, fmt.Errorf("session: attachments: read %s: %w", id, err)
		}
		atts = append(atts, workspace.Attachment{Name: att.Name, MIMEType: att.MIMEType, Data: att.Data})
		names = append(names, att.Name)
	}
	if err := workspace.WriteAttachments(ws, atts); err != nil {
		return nil, fmt.Errorf("session: attachments: %w", err)
	}
	return names, nil
}
