package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"overseer/internal/web/viewmodels"
)

// Wire forms of /api/v1/system/config (the handlers consume the API, not
// the services).
type dbSettingsView struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Name            string `json:"name"`
	User            string `json:"user"`
	SSLMode         string `json:"ssl_mode"`
	MaxConns        int    `json:"max_conns"`
	MinConns        int    `json:"min_conns"`
	ApplicationName string `json:"application_name"`
}

type databaseView struct {
	File               dbSettingsView    `json:"file"`
	Effective          dbSettingsView    `json:"effective"`
	Overrides          map[string]string `json:"overrides"`
	PasswordConfigured bool              `json:"password_configured"`
	PasswordSource     string            `json:"password_source"`
	RestartRequired    bool              `json:"restart_required"`
	ConfigPath         string            `json:"config_path"`
}

type databaseTestResult struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error"`
	ServerVersion string `json:"server_version"`
	SchemaVersion int    `json:"schema_version"`
	DurationMS    int64  `json:"duration_ms"`
}

type providerView struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Endpoint         string   `json:"endpoint"`
	SecretEnv        string   `json:"secret_env"`
	SecretFile       string   `json:"secret_file"`
	Models           []string `json:"models"`
	AllowedRoots     []string `json:"allowed_roots"`
	ConnectTimeout   string   `json:"connect_timeout"`
	SessionTimeout   string   `json:"session_timeout"`
	MaxConcurrent    int      `json:"max_concurrent"`
	Timeout          string   `json:"timeout"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	ProviderType     string   `json:"provider_type"`
	SecretConfigured bool     `json:"secret_configured"`
	UsedBy           []string `json:"used_by"`
}

type providersView struct {
	Workers     []providerView `json:"workers"`
	Compressors []providerView `json:"compressors"`
	ConfigPath  string         `json:"config_path"`
}

const (
	kindWorker      = "worker"
	kindCompression = "compression"
)

// ---------------------------------------------------------------------------
// Global configuration categories: System (database connection) and Models
// (worker and compression providers). Both edit the server configuration
// file through /api/v1/system/config (audited there).

type systemConfigData struct {
	Database *databaseView
	Form     map[string]string
	Test     *databaseTestResult
	SSLModes []string
}

func (c *Console) systemConfigData(r *http.Request) systemConfigData {
	d := systemConfigData{SSLModes: []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}}
	var v databaseView
	if resp := c.d.API.Get(r, "/api/v1/system/config/database", &v); resp.OK() {
		d.Database = &v
		e := v.File
		d.Form = map[string]string{"host": e.Host, "port": itoaInt(e.Port), "name": e.Name, "user": e.User, "ssl_mode": e.SSLMode,
			"max_conns": itoaInt(e.MaxConns), "min_conns": itoaInt(e.MinConns), "application_name": e.ApplicationName}
	}
	return d
}

func itoaInt(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// configSystem — GET /configuration/system.
func (c *Console) configSystem(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "config_system", viewmodels.Page{Shell: c.shell(r, "System configuration", "Configuration"), Data: c.systemConfigData(r)})
}

func databaseBody(r *http.Request) map[string]any {
	return map[string]any{"host": strings.TrimSpace(r.PostFormValue("host")), "port": atoi(r.PostFormValue("port")),
		"name": strings.TrimSpace(r.PostFormValue("name")), "user": strings.TrimSpace(r.PostFormValue("user")),
		"ssl_mode": r.PostFormValue("ssl_mode"), "max_conns": atoi(r.PostFormValue("max_conns")), "min_conns": atoi(r.PostFormValue("min_conns")),
		"application_name": strings.TrimSpace(r.PostFormValue("application_name"))}
}

// configSystemSave — POST /configuration/system: the settings apply at
// the next restart.
func (c *Console) configSystemSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPut, "/api/v1/system/config/database", databaseBody(r), "")
	if !resp.OK() {
		d := c.systemConfigData(r)
		d.Form = formValues(r)
		_, msg, fields := resp.Error()
		c.render(w, r, resp.Status, "config_system", viewmodels.Page{Shell: c.shell(r, "System configuration", "Configuration"), Data: d,
			Errors: fields, Message: msg})
		return
	}
	redirect(w, r, "/configuration/system", "Database settings saved. They apply when the overseer restarts.")
}

// configSystemTest — POST /configuration/system/test: connects with the
// form's settings (and a password typed in for this test only).
func (c *Console) configSystemTest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	body := databaseBody(r)
	if p := r.PostFormValue("password"); p != "" {
		body["password"] = p
	}
	d := c.systemConfigData(r)
	d.Form = formValues(r)
	delete(d.Form, "password")
	var res databaseTestResult
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/system/config/database/test", body, "")
	if resp.OK() && resp.Decode(&res) == nil {
		d.Test = &res
	} else {
		_, msg, _ := resp.Error()
		d.Test = &databaseTestResult{Error: msg}
	}
	c.render(w, r, http.StatusOK, "config_system", viewmodels.Page{Shell: c.shell(r, "System configuration", "Configuration"), Data: d})
}

// configModels — GET /configuration/models.
func (c *Console) configModels(w http.ResponseWriter, r *http.Request) {
	var v providersView
	c.d.API.Get(r, "/api/v1/system/config/providers", &v)
	c.render(w, r, http.StatusOK, "config_models", viewmodels.Page{Shell: c.shell(r, "Models", "Configuration"), Data: v})
}

type modelFormData struct {
	Kind     string
	KindName string
	New      bool
	Name     string
	Provider *providerView
	Form     map[string]string
}

func kindName(kind string) string {
	if kind == kindCompression {
		return "compressor"
	}
	return "worker"
}

func validKind(kind string) bool {
	return kind == kindWorker || kind == kindCompression
}

func providerForm(p providerView) map[string]string {
	return map[string]string{"name": p.Name, "endpoint": p.Endpoint, "secret_env": p.SecretEnv, "secret_file": p.SecretFile,
		"models": strings.Join(p.Models, "\n"), "allowed_roots": strings.Join(p.AllowedRoots, "\n"), "connect_timeout": p.ConnectTimeout,
		"session_timeout": p.SessionTimeout, "max_concurrent": itoaInt(p.MaxConcurrent), "timeout": p.Timeout,
		"max_output_tokens": itoaInt(p.MaxOutputTokens)}
}

// configModelNew — GET /configuration/models/{kind}/new.
func (c *Console) configModelNew(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKind(kind) {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "Unknown provider kind.")
		return
	}
	form := map[string]string{}
	if kind == kindWorker {
		form["endpoint"] = "http://"
	}
	c.render(w, r, http.StatusOK, "config_model", viewmodels.Page{Shell: c.shell(r, "Add "+kindName(kind)+" provider", "Configuration"),
		Data: modelFormData{Kind: kind, KindName: kindName(kind), New: true, Form: form}})
}

func (c *Console) findProvider(r *http.Request, kind, name string) *providerView {
	var v providersView
	c.d.API.Get(r, "/api/v1/system/config/providers", &v)
	list := v.Workers
	if kind == kindCompression {
		list = v.Compressors
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// configModel — GET /configuration/models/{kind}/{name}.
func (c *Console) configModel(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	p := c.findProvider(r, kind, name)
	if !validKind(kind) || p == nil {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "Provider not found.")
		return
	}
	c.render(w, r, http.StatusOK, "config_model", viewmodels.Page{Shell: c.shell(r, "Provider "+name, "Configuration"),
		Data: modelFormData{Kind: kind, KindName: kindName(kind), Name: name, Provider: p, Form: providerForm(*p)}})
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// configModelSave — POST /configuration/models/{kind} (new; name from the
// form) or /configuration/models/{kind}/{name}: applies immediately.
func (c *Console) configModelSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	isNew := name == ""
	if isNew {
		name = strings.TrimSpace(r.PostFormValue("name"))
	}
	if !validKind(kind) {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "Unknown provider kind.")
		return
	}
	body := map[string]any{"endpoint": strings.TrimSpace(r.PostFormValue("endpoint")), "secret_env": strings.TrimSpace(r.PostFormValue("secret_env")),
		"secret_file": strings.TrimSpace(r.PostFormValue("secret_file"))}
	if kind == kindWorker {
		body["models"], body["allowed_roots"] = lines(r.PostFormValue("models")), lines(r.PostFormValue("allowed_roots"))
		body["connect_timeout"], body["session_timeout"] = strings.TrimSpace(r.PostFormValue("connect_timeout")), strings.TrimSpace(r.PostFormValue("session_timeout"))
		body["max_concurrent"] = atoi(r.PostFormValue("max_concurrent"))
	} else {
		body["timeout"], body["max_output_tokens"] = strings.TrimSpace(r.PostFormValue("timeout")), atoi(r.PostFormValue("max_output_tokens"))
	}
	if isNew && c.findProvider(r, kind, name) != nil {
		c.render(w, r, http.StatusConflict, "config_model", viewmodels.Page{Shell: c.shell(r, "Add "+kindName(kind)+" provider", "Configuration"),
			Data:   modelFormData{Kind: kind, KindName: kindName(kind), New: true, Form: formValues(r)},
			Errors: map[string]string{"name": "a " + kindName(kind) + " provider with this name exists"}, Message: "Not saved."})
		return
	}
	resp := c.d.API.Do(r, http.MethodPut, "/api/v1/system/config/providers/"+url.PathEscape(kind)+"/"+url.PathEscape(name), body, "")
	if !resp.OK() {
		_, msg, fields := resp.Error()
		d := modelFormData{Kind: kind, KindName: kindName(kind), New: isNew, Name: name, Form: formValues(r)}
		if !isNew {
			d.Provider = c.findProvider(r, kind, name)
		}
		c.render(w, r, resp.Status, "config_model", viewmodels.Page{Shell: c.shell(r, "Provider "+name, "Configuration"), Data: d,
			Errors: fields, Message: msg})
		return
	}
	redirect(w, r, "/configuration/models", "Provider "+name+" saved and applied.")
}

// configModelDelete — POST /configuration/models/{kind}/{name}/delete.
func (c *Console) configModelDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Deleting a provider requires confirmation.")
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	resp := c.d.API.Do(r, http.MethodDelete, "/api/v1/system/config/providers/"+url.PathEscape(kind)+"/"+url.PathEscape(name), nil, "")
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/configuration/models", "Provider "+name+" deleted.")
}
