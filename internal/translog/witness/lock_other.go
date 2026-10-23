//go:build !unix

package witness

import (
	"context"
	"errors"
)

// lockDir needs flock: the witness runs on a unix machine.
func lockDir(context.Context, string) (func(), error) {
	return nil, errors.New("witness: the directory lock needs a unix system (flock)")
}
