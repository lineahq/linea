package worker

import (
	"fmt"
	"strconv"
	"strings"
)

// ChildUser is the user and group a pool starts runtime children as, and a
// builder starts a built worker as for its hello check. A child under
// another uid, with no supplementary groups, cannot read the parent's
// environment or memory, nor a token file only the parent's uid may read.
// It is a Linux feature: elsewhere Apply fails and isolation is the
// operator's.
type ChildUser struct {
	UID, GID uint32
}

// ParseChildUser reads "UID:GID", both numeric and not 0.
func ParseChildUser(s string) (*ChildUser, error) {
	u, g, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("child user %q: want UID:GID", s)
	}
	uid, err := strconv.ParseUint(u, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("child user %q: the UID is not a number", s)
	}
	gid, err := strconv.ParseUint(g, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("child user %q: the GID is not a number", s)
	}
	if uid == 0 || gid == 0 {
		return nil, fmt.Errorf("child user %q: children never run as root", s)
	}
	return &ChildUser{UID: uint32(uid), GID: uint32(gid)}, nil
}

func (u ChildUser) String() string { return fmt.Sprintf("%d:%d", u.UID, u.GID) }

// TokenFileExposed reports why u may read the token file at path, or ""
// when it may not. It always reports "" outside Linux.
func TokenFileExposed(path string, u ChildUser) (string, error) {
	return exposedTo(path, u)
}
