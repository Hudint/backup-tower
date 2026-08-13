package source

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/archive"
)

// RestoreOptions configures writing data back into a mount.
type RestoreOptions struct {
	// Clean empties the destination before extracting. A restore has to replace,
	// not merge: files created after the snapshot would otherwise survive it and
	// leave the application looking at a state that never existed.
	Clean bool
	// Chown restores recorded ownership. Inside the helper container this works
	// because it runs as root; on the direct path it needs privileges.
	Chown bool
}

// Restore writes an archive stream back into a mount and reports how it got
// there.
func (a *Accessor) Restore(ctx context.Context, m runtime.Mount, r io.Reader, opts RestoreOptions) (Method, error) {
	method := a.chooseWriteMethod(m)
	switch method {
	case MethodDirect:
		if opts.Clean {
			if err := archive.CleanDir(m.Source); err != nil {
				return MethodDirect, err
			}
		}
		err := archive.Extract(ctx, r, m.Source, archive.ExtractOptions{Chown: opts.Chown})
		return MethodDirect, err
	case MethodHelper:
		return MethodHelper, a.restoreViaHelper(ctx, m, r, opts)
	default:
		return method, fmt.Errorf("unknown access method %q", method)
	}
}

// chooseWriteMethod is stricter than its read counterpart: being able to list a
// directory says nothing about being able to write into it, and discovering the
// difference halfway through an extraction would leave the volume half-restored.
func (a *Accessor) chooseWriteMethod(m runtime.Mount) Method {
	if a.opts.Force != "" {
		return a.opts.Force
	}
	if m.Source == "" {
		return MethodHelper
	}
	probe, err := os.CreateTemp(m.Source, ".backup-tower-write-probe-*")
	if err != nil {
		return MethodHelper
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return MethodDirect
}

func (a *Accessor) restoreViaHelper(ctx context.Context, m runtime.Mount, r io.Reader, opts RestoreOptions) error {
	if a.opts.HelperImage == "" {
		return fmt.Errorf("cannot write to %s directly and no helper image is configured", describe(m))
	}

	bind, err := helperBind(m, HelperRestoreMount, true)
	if err != nil {
		return err
	}

	cmd := []string{"helper", "extract", "--dest", HelperRestoreMount}
	if opts.Clean {
		cmd = append(cmd, "--clean")
	}
	if opts.Chown {
		cmd = append(cmd, "--chown")
	}

	_, err = a.rt.RunHelper(ctx, runtime.HelperSpec{
		Image:   a.opts.HelperImage,
		Cmd:     cmd,
		Binds:   []string{bind},
		Purpose: "restoring " + describe(m),
		Stdin:   r,
		// Extraction writes only into the mounted destination, but the tar
		// reader needs a writable temp area on some paths.
		WritableRootfs: false,
	}, nil)
	return err
}
