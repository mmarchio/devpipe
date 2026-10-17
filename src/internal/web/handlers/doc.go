// Package handlers serves the server-rendered operations console pages
// (WBS 17.1). Handlers depend only on consumer-side interfaces declared in
// this package and on internal/readmodel DTOs; they never import
// internal/service, persistence adapters or worker gateways (archtest).
package handlers
