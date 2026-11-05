package artifact

import (
	"context"
	"fmt"
	"time"
)

// MaxRoleBindings bounds the managed role bindings.
const MaxRoleBindings = 1000

// RoleBindings holds the role bindings managed through the platform, beside
// the ones a server config declares. The set has one revision that every write
// raises; a write expecting another revision is ErrConflict, so an edit made
// from a stale list fails. Every write records a ChangeRoleBindings.
type RoleBindings interface {
	// RoleBindings returns the managed bindings, oldest first, and the
	// set's revision, 0 before the first write.
	RoleBindings(ctx context.Context) (RoleBindingSet, error)
	// AddRoleBinding adds b at revision expected. A binding with the same
	// subject, role and scope, or an existing ID, is ErrConflict; a binding
	// beyond MaxRoleBindings is ErrLimit.
	AddRoleBinding(ctx context.Context, expected uint64, b RoleBindingRecord) (RoleBindingSet, error)
	// RemoveRoleBinding removes binding id at revision expected, or is
	// ErrNotFound.
	RemoveRoleBinding(ctx context.Context, expected uint64, id string) (RoleBindingSet, error)
}

// RoleBindingRecord grants Role in Scope to exactly one of Group and
// Principal. The platform validates the role and scope; the repository keeps
// them as text.
type RoleBindingRecord struct {
	ID        string
	Group     string
	Principal string
	Role      string
	Scope     string
	CreatedBy string
	Created   time.Time
}

// RoleBindingSet is the managed bindings at one revision.
type RoleBindingSet struct {
	Revision uint64
	Bindings []RoleBindingRecord
}

// ValidateRoleBinding reports why a repository refuses b, as
// ErrInvalidRequest: an ID of 32 lowercase hex digits, exactly one subject
// of 1 to 256 printable characters, a role of 1 to 64 and a scope of 1 to
// 256 printable characters, a printable creator of at most 256 characters
// and a creation time.
func ValidateRoleBinding(b RoleBindingRecord) error {
	subject := b.Group + b.Principal
	switch {
	case !lowerHex(b.ID, 32):
		return fmt.Errorf("%w: role binding ID must be 32 lowercase hex digits", ErrInvalidRequest)
	case (b.Group == "") == (b.Principal == ""):
		return fmt.Errorf("%w: role binding names exactly one group or principal", ErrInvalidRequest)
	case len(subject) > 256 || !printable(subject):
		return fmt.Errorf("%w: role binding subject must be 1 to 256 printable characters", ErrInvalidRequest)
	case b.Role == "" || len(b.Role) > 64 || !printable(b.Role):
		return fmt.Errorf("%w: role binding role must be 1 to 64 printable characters", ErrInvalidRequest)
	case b.Scope == "" || len(b.Scope) > 256 || !printable(b.Scope):
		return fmt.Errorf("%w: role binding scope must be 1 to 256 printable characters", ErrInvalidRequest)
	case len(b.CreatedBy) > 256 || !printable(b.CreatedBy):
		return fmt.Errorf("%w: role binding creator too long or not printable", ErrInvalidRequest)
	case b.Created.IsZero():
		return fmt.Errorf("%w: role binding needs a creation time", ErrInvalidRequest)
	}
	return nil
}

// SameGrant reports whether a and b grant the same role in the same scope to
// the same subject.
func (a RoleBindingRecord) SameGrant(b RoleBindingRecord) bool {
	return a.Group == b.Group && a.Principal == b.Principal && a.Role == b.Role && a.Scope == b.Scope
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
