// Package e2e contains end-to-end tests that run the real application
// (internal/app) on a local port and use it like a browser: the setup
// wizard (also from another computer, with the setup token), the admin
// pages, single sign-on against an in-process OpenID provider
// (internal/testutil/mockidp), incidents and maintenance windows with e-mail
// notifications to a fake SMTP server (internal/testutil/fakesmtp), a
// restart, HTTPS with tls_cert/tls_key, and the command line tool.
//
// Two more journeys cover the Microsoft Entra ID integration: SCIM
// provisioning (requests shaped like those of the Entra provisioning
// service; provisioned users are notified, sign in and are deactivated) and
// e-mail with Microsoft Graph against a fake token and sendMail endpoint
// (internal/testutil/fakegraph).
//
// The main journey and the SCIM journey run with SQLite and, when
// STATUSPAGE_TEST_PG_DSN is set, also with PostgreSQL (the public schema of
// that database is dropped and recreated); the other tests use SQLite.
//
// The package has no non-test code.
package e2e
