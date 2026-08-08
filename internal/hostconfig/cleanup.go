package hostconfig

import (
	"context"
	"fmt"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

// CleanupManagedForUninstall removes only the canonical rule at VoHive's fixed
// managed path. Production callers pass nil so no path or target can be
// supplied by the command line. A manager may be injected by tests.
func CleanupManagedForUninstall(
	ctx context.Context,
	manager *mmisolation.Manager,
) (mmisolation.Result, error) {
	var err error
	if manager == nil {
		manager, err = mmisolation.New(mmisolation.Options{})
		if err != nil {
			return mmisolation.Result{}, err
		}
	}

	status, err := manager.Inspect()
	if err != nil {
		return mmisolation.Result{}, err
	}
	result := mmisolation.Result{
		State: status.State, Revision: status.Revision, Entries: status.Entries,
	}
	switch status.State {
	case mmisolation.StateAbsent:
		return result, nil
	case mmisolation.StateManaged:
		return manager.Uninstall(ctx, mmisolation.Request{
			ExpectedRevision: status.Revision,
		})
	case mmisolation.StateForeign:
		return result, fmt.Errorf("%w: %s", mmisolation.ErrForeignRule, status.Reason)
	case mmisolation.StateTampered:
		return result, fmt.Errorf("%w: %s", mmisolation.ErrTamperedRule, status.Reason)
	default:
		return result, fmt.Errorf(
			"%w: unknown rule state %q", mmisolation.ErrInvalidRequest, status.State,
		)
	}
}
