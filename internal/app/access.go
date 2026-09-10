package app

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Access string

const (
	AccessViewer Access = "viewer"
	AccessEditor Access = "editor"
	AccessOwner  Access = "owner"
)

type WorkspaceGrant struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Access Access `json:"access"`
}

type AccessManager struct {
	grants map[uint64]map[string]WorkspaceGrant
	locks  sync.Map
}

func NewAccessManager(config Config) *AccessManager {
	m := &AccessManager{grants: make(map[uint64]map[string]WorkspaceGrant)}
	for _, workspace := range config.Workspaces {
		for _, member := range workspace.Members {
			if m.grants[member.UserID] == nil {
				m.grants[member.UserID] = make(map[string]WorkspaceGrant)
			}
			m.grants[member.UserID][workspace.ID] = WorkspaceGrant{ID: workspace.ID, Name: workspace.Name, Access: Access(member.Access)}
		}
	}
	return m
}

func (m *AccessManager) List(userID uint64) []WorkspaceGrant {
	grants := m.grants[userID]
	result := make([]WorkspaceGrant, 0, len(grants)+1)
	result = append(result, WorkspaceGrant{ID: fmt.Sprintf("user-%d", userID), Name: "Personal workspace", Access: AccessOwner})
	for _, grant := range grants {
		result = append(result, grant)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *AccessManager) Grant(userID uint64, workspace string) (WorkspaceGrant, error) {
	if workspace == "" {
		workspace = fmt.Sprintf("user-%d", userID)
	}
	if workspace == fmt.Sprintf("user-%d", userID) {
		return WorkspaceGrant{ID: workspace, Name: "Personal workspace", Access: AccessOwner}, nil
	}
	grant, ok := m.grants[userID][workspace]
	if !ok {
		return WorkspaceGrant{}, fmt.Errorf("you do not have access to workspace %q", workspace)
	}
	return grant, nil
}

func (m *AccessManager) Lock(id string) *sync.RWMutex {
	lock, _ := m.locks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}
func validWorkspaceID(id string) bool {
	if id == "" || strings.HasPrefix(id, "user-") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func validAccess(access string) bool {
	return access == string(AccessViewer) || access == string(AccessEditor) || access == string(AccessOwner)
}
func canWrite(access Access) bool  { return access == AccessEditor || access == AccessOwner }
func canDelete(access Access) bool { return access == AccessOwner }
