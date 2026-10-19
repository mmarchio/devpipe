package web

import (
	"net/http"

	"overseer/internal/access"
	"overseer/internal/service"
)

// RegisterSystemConfigRoutes exposes the console's System and Models
// configuration categories:
//
//	GET    /api/v1/system/config/database
//	PUT    /api/v1/system/config/database                      (applies at restart)
//	POST   /api/v1/system/config/database/test                 (connection test; password used once)
//	GET    /api/v1/system/config/providers
//	PUT    /api/v1/system/config/providers/{kind}/{name}       (create / update; applies now)
//	DELETE /api/v1/system/config/providers/{kind}/{name}       (applies now)
func RegisterSystemConfigRoutes(r *Router, svc *service.SystemConfigService) error {
	a := &systemConfigAPI{svc: svc}
	routes := []struct {
		method, pattern string
		action          access.Action
		h               http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/system/config/database", access.ActionRead, a.database},
		{http.MethodPut, "/api/v1/system/config/database", access.ActionMaintenance, a.saveDatabase},
		{http.MethodPost, "/api/v1/system/config/database/test", access.ActionMaintenance, a.testDatabase},
		{http.MethodGet, "/api/v1/system/config/providers", access.ActionRead, a.providers},
		{http.MethodPut, "/api/v1/system/config/providers/{kind}/{name}", access.ActionMaintenance, a.saveProvider},
		{http.MethodDelete, "/api/v1/system/config/providers/{kind}/{name}", access.ActionMaintenance, a.deleteProvider},
	}
	for _, rt := range routes {
		if err := r.RegisterAPIAction(rt.method, rt.pattern, rt.action, rt.h); err != nil {
			return err
		}
	}
	return nil
}

type systemConfigAPI struct {
	svc *service.SystemConfigService
}

var databaseFields = map[string]bool{"host": true, "port": true, "name": true, "user": true, "ssl_mode": true, "max_conns": true,
	"min_conns": true, "application_name": true}

func (a *systemConfigAPI) database(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Database(r.Context())
	respond(w, r, v, err)
}

func (a *systemConfigAPI) saveDatabase(w http.ResponseWriter, r *http.Request) {
	var in service.DatabaseSettings
	if err := decodeStrictJSON(r, CodeInvalidInput, databaseFields, []string{"host", "port", "name", "user"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.svc.SaveDatabase(r.Context(), in)
	respond(w, r, v, err)
}

func (a *systemConfigAPI) testDatabase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		service.DatabaseSettings
		// Password is used for this test only and never stored.
		Password string `json:"password"`
	}
	allowed := map[string]bool{"password": true}
	for k := range databaseFields {
		allowed[k] = true
	}
	if err := decodeStrictJSON(r, CodeInvalidInput, allowed, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.svc.TestDatabase(r.Context(), in.DatabaseSettings, in.Password))
}

func (a *systemConfigAPI) providers(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Providers(r.Context())
	respond(w, r, v, err)
}

var providerFields = map[string]bool{"endpoint": true, "secret_env": true, "secret_file": true, "models": true, "allowed_roots": true,
	"connect_timeout": true, "session_timeout": true, "max_concurrent": true, "timeout": true, "max_output_tokens": true}

func (a *systemConfigAPI) saveProvider(w http.ResponseWriter, r *http.Request) {
	var in service.ProviderSettings
	if err := decodeStrictJSON(r, CodeInvalidInput, providerFields, []string{"endpoint"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	in.Kind, in.Name = r.PathValue("kind"), r.PathValue("name")
	v, err := a.svc.SaveProvider(r.Context(), in)
	respond(w, r, v, err)
}

func (a *systemConfigAPI) deleteProvider(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.DeleteProvider(r.Context(), r.PathValue("kind"), r.PathValue("name"))
	respond(w, r, v, err)
}
