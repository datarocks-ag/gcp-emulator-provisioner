package provisioner

import (
	"context"

	"gcp-emulator-provisioner/internal/client"
)

// applyPaths sends an update mask in one call and, if this endpoint refuses one
// of the paths, re-sends each path on its own.
//
// The Pub/Sub emulator rejects update masks that Google Cloud accepts, and it
// validates the whole mask before touching the resource — so a single
// unsupported path abandons every other change batched with it. Verified
// against cloud-sdk:emulators: an update naming `ack_deadline_seconds` and
// `labels` together leaves the ack deadline unchanged, and the run still
// reports success. Retrying path by path applies what this endpoint can take
// and names precisely what it could not.
//
// The batch is attempted first so the common case, and every run against Google
// Cloud, still costs a single call.
//
// onUnsupported is called once per refused path; any other error aborts.
func applyPaths(
	ctx context.Context,
	paths []string,
	update func(ctx context.Context, mask []string) error,
	onUnsupported func(path string, err error),
) error {
	err := update(ctx, paths)
	if err == nil {
		return nil
	}
	if !client.IsUnsupportedField(err) {
		return err
	}

	// A one-path batch has nothing left to split: the refused path is the batch.
	if len(paths) == 1 {
		onUnsupported(paths[0], err)
		return nil
	}

	for _, path := range paths {
		switch err := update(ctx, []string{path}); {
		case err == nil:
		case client.IsUnsupportedField(err):
			onUnsupported(path, err)
		default:
			return err
		}
	}
	return nil
}
