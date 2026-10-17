package web

import (
	"net/http"

	"overseer/internal/role"
	"overseer/internal/service"
)

type promptSnapshotResource struct {
	Version     int    `json:"version"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
}

func toPromptSnapshotResource(p role.PromptSnapshot) promptSnapshotResource {
	return promptSnapshotResource{Version: p.Version, Content: p.Content, ContentHash: p.ContentHash}
}

type instructionSnapshotResource struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
}

func toInstructionSnapshotResource(i role.InstructionSnapshot) instructionSnapshotResource {
	return instructionSnapshotResource{Key: i.Key, Name: i.Name, Version: i.Version, Content: i.Content, ContentHash: i.ContentHash}
}

// RegisterPreviewRoutes exposes the role preview endpoint (WBS 5.27):
//
//	GET /api/v1/projects/{project}/roles/{role}/preview
//
// The response is the read-model view of what currently constitutes the role:
// system prompt, ordered instructions, allowed states, state files, and a
// configuration hash derived at read time. No prompt composition, no mutation.
func RegisterPreviewRoutes(r *Router, svc *service.RoleService) error {
	api := &roleAPI{svc: svc}
	if err := r.RegisterAPI(http.MethodGet, "/api/v1/projects/{project}/roles/{role}/preview", api.handlePreview); err != nil {
		return err
	}
	return nil
}

func (a *roleAPI) handlePreview(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.Preview(r.Context(), r.PathValue("role"))
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	instructions := make([]instructionSnapshotResource, 0, len(out.Instructions))
	for _, i := range out.Instructions {
		instructions = append(instructions, toInstructionSnapshotResource(i))
	}
	body := map[string]any{
		"key":                out.Key,
		"name":               out.Name,
		"version":            out.Version,
		"system_prompt":      toPromptSnapshotResource(out.SystemPrompt),
		"instructions":       instructions,
		"allowed_states":     out.AllowedStates,
		"state_files":        out.StateFiles,
		"configuration_hash": out.ConfigurationHash,
	}
	writeJSON(w, http.StatusOK, body)
}
