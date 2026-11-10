package platform

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
)

// The built-in admin account. A server composed with an admin
// password has one user, admin, who signs in with that password and is admin
// on scope *, so one person can run a server before any identity provider is
// connected. The password comes from the server's configuration and is never
// stored: the account compares SHA-256 sums in constant time. Without a
// password the account is off and its sessions and API tokens are refused,
// so turning it off needs no cleanup.

// AdminName is the built-in admin account's user name.
const AdminName = "admin"

// MinAdminPassword is the shortest admin password accepted, in bytes.
const MinAdminPassword = 16

// adminIssuer is the identity source of the built-in account in its ID;
// no identity provider issues as "linea".
const adminIssuer = "linea"

// AdminPrincipalID is the built-in admin account's principal ID.
var AdminPrincipalID = UserID(adminIssuer, AdminName)

var adminBinding = authz.Binding{Subject: authz.Subject{Principal: AdminPrincipalID}, Role: "admin", Scope: authz.All}

// Failed sign-ins are bounded per instance: per client address and in total,
// in fixed windows.
const (
	adminFailureWindow     = time.Minute
	adminFailuresPerClient = 5
	adminFailuresTotal     = 20
	adminTrackedClients    = 1024
)

var (
	// ErrAdminPassword refuses a wrong admin password.
	ErrAdminPassword = fmt.Errorf("%w: wrong user name or password", ErrUnauthenticated)
	// ErrAdminThrottled refuses a sign-in after too many failures.
	ErrAdminThrottled = errors.New("too many failed sign-ins; try again in a minute")
	errAdminOff       = fmt.Errorf("%w: the built-in admin account is off", ErrUnauthenticated)
)

func checkAdminOptions(opts Options) error {
	switch {
	case opts.AdminPassword == "":
		return nil
	case opts.Access == nil:
		return errors.New("the built-in admin account needs a server's access policy")
	case len(opts.AdminPassword) < MinAdminPassword:
		return fmt.Errorf("the admin password needs at least %d characters", MinAdminPassword)
	}
	if _, err := opts.Access.Extend([]authz.Binding{adminBinding}); err != nil {
		return fmt.Errorf("the built-in admin account's binding: %w", err)
	}
	return nil
}

type adminAccount struct {
	sum [sha256.Size]byte
	now func() time.Time

	mu       sync.Mutex
	window   time.Time
	total    int
	byClient map[string]int
}

func newAdminAccount(password string) *adminAccount {
	return &adminAccount{sum: sha256.Sum256([]byte(password)), now: time.Now, byClient: map[string]int{}}
}

func adminPrincipal() Principal {
	return Principal{ID: AdminPrincipalID, Kind: PrincipalUser, Name: AdminName}
}

// check verifies a sign-in from client, an address the adapter saw. It
// refuses without comparing once client or the instance failed too often.
func (a *adminAccount) check(client, user, password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if now.Sub(a.window) >= adminFailureWindow {
		a.window, a.total = now, 0
		clear(a.byClient)
	}
	if a.total >= adminFailuresTotal || a.byClient[client] >= adminFailuresPerClient {
		return ErrAdminThrottled
	}
	sum := sha256.Sum256([]byte(password))
	ok := subtle.ConstantTimeCompare(sum[:], a.sum[:]) == 1
	if ok && user == AdminName {
		return nil
	}
	a.total++
	if _, seen := a.byClient[client]; seen || len(a.byClient) < adminTrackedClients {
		a.byClient[client]++
	}
	return ErrAdminPassword
}

// enabledPrincipal refuses the built-in admin account's credentials while
// the account is off.
func (s *Service) enabledPrincipal(id string) error {
	if id == AdminPrincipalID && s.admin == nil {
		return errAdminOff
	}
	return nil
}
