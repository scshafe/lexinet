package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	// DatabasePasswordEnv and DatabasePasswordFileEnv supply the database
	// password without writing it into the config file (which `config set`
	// would otherwise round-trip). They are deliberately not TS_* variables.
	DatabasePasswordEnv     = "LEXINET_DB_PASSWORD"
	DatabasePasswordFileEnv = "LEXINET_DB_PASSWORD_FILE"

	maximumDatabasePasswordBytes = 1024
)

// DatabaseDSN returns the configured DSN with the password from
// LEXINET_DB_PASSWORD or LEXINET_DB_PASSWORD_FILE applied. It never changes the
// stored configuration. A DSN that already carries a password, or has no user,
// is returned unchanged; setting both variables is an error. Errors never
// include the password.
func (c Config) DatabaseDSN() (string, error) {
	return databaseDSNWithPassword(c.Service.Database.DSN, os.LookupEnv)
}

func databaseDSNWithPassword(dsn string, lookup func(string) (string, bool)) (string, error) {
	password, hasEnv := lookup(DatabasePasswordEnv)
	file, hasFile := lookup(DatabasePasswordFileEnv)
	hasEnv = hasEnv && password != ""
	hasFile = hasFile && file != ""
	switch {
	case hasEnv && hasFile:
		return "", fmt.Errorf("set only one of %s and %s", DatabasePasswordEnv, DatabasePasswordFileEnv)
	case hasFile:
		data, err := readPasswordFile(file)
		if err != nil {
			return "", err
		}
		password = data
	case !hasEnv:
		return dsn, nil
	}
	if password == "" {
		return "", errors.New("database password is empty")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil || parsed.User.Username() == "" {
		return dsn, nil
	}
	if _, has := parsed.User.Password(); has {
		return dsn, nil
	}
	parsed.User = url.UserPassword(parsed.User.Username(), password)
	return parsed.String(), nil
}

func readPasswordFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumDatabasePasswordBytes {
		return "", fmt.Errorf("%s must name a regular file of at most %d bytes", DatabasePasswordFileEnv, maximumDatabasePasswordBytes)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s file must not be group or world accessible", DatabasePasswordFileEnv)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s file is unreadable", DatabasePasswordFileEnv)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
