//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package sqlitemanagedsubscriptions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mariotoffia/gobridge/domain/shared"
	"golang.org/x/sys/unix"
)

type sqliteACL struct {
	path        string
	base        string
	dirFD       int
	dirDev      uint64
	dirIno      uint64
	preexisting map[string]bool
}

func (a *sqliteACL) close() {
	if a != nil && a.dirFD >= 0 {
		_ = unix.Close(a.dirFD)
		a.dirFD = -1
	}
}

func prepareSQLiteACL(ctx context.Context, dbPath string) (*sqliteACL, error) {
	if err := validateSQLitePath(dbPath); err != nil {
		return nil, err
	}
	parent := filepath.Dir(dbPath)
	dirFD, stat, err := openSecureSQLiteParent(ctx, parent)
	if err != nil {
		return nil, err
	}
	acl := &sqliteACL{
		path: dbPath, base: filepath.Base(dbPath), dirFD: dirFD,
		dirDev: uint64(stat.Dev), dirIno: uint64(stat.Ino),
		preexisting: make(map[string]bool, 4),
	}
	failed := true
	defer func() {
		if failed {
			acl.close()
		}
	}()

	for _, name := range acl.names() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		exists, inspectErr := acl.inspectRelative(name, false)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if exists {
			acl.preexisting[name] = true
			continue
		}
		if name != acl.base {
			continue
		}
		fd, createErr := unix.Openat(acl.dirFD, name,
			unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if createErr != nil {
			return nil, unavailable("create managed subscription SQLite database", createErr)
		}
		if chmodErr := unix.Fchmod(fd, 0o600); chmodErr != nil {
			_ = unix.Close(fd)
			return nil, unavailable("secure managed subscription SQLite database", chmodErr)
		}
		if validateErr := validateSQLiteFileFD(fd, dbPath); validateErr != nil {
			_ = unix.Close(fd)
			return nil, validateErr
		}
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, unavailable("close new managed subscription SQLite database", closeErr)
		}
	}
	failed = false
	return acl, nil
}

func openSecureSQLiteParent(ctx context.Context, parent string) (int, unix.Stat_t, error) {
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, unavailable("open managed subscription SQLite root", err)
	}
	components := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	// Darwin exposes /var as a root-owned compatibility symlink to /private/var.
	// Walk its immutable canonical target descriptor-relatively; all service-local
	// symlink components remain rejected by O_NOFOLLOW below.
	if runtime.GOOS == "darwin" && len(components) > 0 && components[0] == "var" {
		components = append([]string{"private", "var"}, components[1:]...)
	}
	for i, component := range components {
		if component == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, err
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		created := false
		if openErr == unix.ENOENT {
			if mkdirErr := unix.Mkdirat(fd, component, 0o700); mkdirErr != nil {
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, unavailable("create managed subscription SQLite parent", mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			created = true
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("managed subscription SQLite parent component %q must be a non-symlink directory", component)).Wrap(openErr)
		}
		_ = unix.Close(fd)
		fd = next
		if created {
			if chmodErr := unix.Fchmod(fd, 0o700); chmodErr != nil {
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, unavailable("secure managed subscription SQLite parent", chmodErr)
			}
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, unavailable("inspect managed subscription SQLite parent", statErr)
		}
		final := i == len(components)-1
		if validateErr := validateSQLiteDirectory(component, stat, final); validateErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, validateErr
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, unavailable("inspect managed subscription SQLite parent", err)
	}
	return fd, stat, nil
}

func validateSQLiteDirectory(component string, stat unix.Stat_t, final bool) error {
	perm := uint32(stat.Mode) & 0o7777
	uid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != uid {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite parent component %q has an unsafe owner", component))
	}
	if final {
		if stat.Uid != uid || perm != 0o700 {
			return shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("managed subscription SQLite final parent %q must be owned by the process user with permissions 0700", component))
		}
		return nil
	}
	if perm&0o022 != 0 && (perm&0o1000 == 0 || stat.Uid != 0) {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite parent component %q is writable by group or other", component))
	}
	return nil
}

func (a *sqliteACL) names() []string {
	return []string{a.base, a.base + "-wal", a.base + "-shm", a.base + "-journal"}
}

func (a *sqliteACL) inspectRelative(name string, allowTighten bool) (bool, error) {
	fd, err := unix.Openat(a.dirFD, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == unix.ENOENT {
		return false, nil
	}
	if err != nil {
		return false, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q must be regular and must not be a symlink", filepath.Join(filepath.Dir(a.path), name))).Wrap(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := validateSQLiteFileFD(fd, filepath.Join(filepath.Dir(a.path), name)); err != nil {
		if !allowTighten {
			return false, err
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return false, err
		}
		if chmodErr := unix.Fchmod(fd, 0o600); chmodErr != nil {
			return false, unavailable("secure managed subscription SQLite files", chmodErr)
		}
		if verifyErr := validateSQLiteFileFD(fd, filepath.Join(filepath.Dir(a.path), name)); verifyErr != nil {
			return false, verifyErr
		}
	}
	return true, nil
}

func validateSQLiteFileFD(fd int, path string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return unavailable("inspect managed subscription SQLite files", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q must be an owner-controlled regular file", path))
	}
	if stat.Mode&0o777 != 0o600 {
		return shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("managed subscription SQLite file %q has insecure permissions %04o; require 0600", path, stat.Mode&0o777))
	}
	return nil
}

func (a *sqliteACL) verifyHeldParent() error {
	fd, stat, err := openSecureSQLiteParent(context.Background(), filepath.Dir(a.path))
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if uint64(stat.Dev) != a.dirDev || uint64(stat.Ino) != a.dirIno {
		return shared.ErrInvalidConfig.WithMessage("managed subscription SQLite parent changed during initialization")
	}
	return nil
}

func (a *sqliteACL) secureCreatedFiles() error {
	if err := a.verifyHeldParent(); err != nil {
		return err
	}
	for _, name := range a.names() {
		allowTighten := !a.preexisting[name]
		if _, err := a.inspectRelative(name, allowTighten); err != nil {
			return err
		}
	}
	return nil
}
