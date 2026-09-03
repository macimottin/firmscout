package application

import (
	"errors"
	"io"

	"github.com/macimottin/firmscout/internal/domain"
)

// isNotFound reports whether err signals a missing entity. Adapters translate their
// library errors into domain.ErrNotFound at their boundary, so this check works
// regardless of which adapter produced it.
func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}
