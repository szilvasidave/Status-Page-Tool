// Package e2e contains end-to-end tests that run the real application
// (internal/app) on a local port and use it like a browser: the setup
// wizard, the admin pages, single sign-on against an in-process OpenID
// provider (internal/testutil/mockidp), incidents and maintenance windows
// with e-mail notifications to a fake SMTP server
// (internal/testutil/fakesmtp), a restart, and the command line tool.
//
// The journey runs with SQLite and, when STATUSPAGE_TEST_PG_DSN is set, with
// PostgreSQL (the public schema of that database is dropped and recreated).
//
// The package has no non-test code.
package e2e
