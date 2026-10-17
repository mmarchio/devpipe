// Package service is the application layer between the HTTP boundary and
// persistence (WBS 3.1). It performs domain validation, coordinates
// transaction boundaries, and translates repository failures into stable
// service-level errors. HTTP handlers must not coordinate repositories
// directly; they call the services here.
package service
