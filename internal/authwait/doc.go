// Package authwait holds the guard that keeps startup dials to yarilo-auth
// bounded by a wait rather than by an immediate exit (#1369). It has no
// runtime code: the waiting lives in the two auth client packages, and what
// lives here is the rule that every startup site uses it.
package authwait
