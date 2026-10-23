package agent

import "fmt"

func (a *Agent) checkWorkspace(id string) error {
	if id == "" {
		return nil
	}
	w, err := a.DB.Workspace(id)
	if err != nil {
		return fmt.Errorf("agent: workspace %s not found", id)
	}
	if w.ArchivedAt != nil {
		return fmt.Errorf("agent: workspace %s is archived; restore it before starting new work", w.Name)
	}
	return nil
}
