package neoread

import (
	"encoding/json"
	"errors"
	"strings"
)

func validateLibrarySyncRow(table, recordKey string, row syncRow) error {
	if table != "library_chat_runs" && row["document_id"] != libraryChatOwner {
		return errors.New("云端书库对话归属无效")
	}
	session := strings.TrimPrefix(recordKey, "chat/")
	switch table {
	case "library_chat_sessions":
		var scope libraryChatScope
		if row["id"] != session || !strings.HasPrefix(session, "library-") || !syncID(strings.TrimPrefix(session, "library-")) || json.Unmarshal([]byte(fmtSyncString(row["scope"])), &scope) != nil || scope.Category == "" || len(scope.DocumentIDs) == 0 || !validAgent(fmtSyncString(row["provider"])) {
			return errors.New("云端书库对话范围无效")
		}
		for _, id := range scope.DocumentIDs {
			if !safeSyncID(id) {
				return errors.New("云端书库对话论文标识无效")
			}
		}
	case "library_messages":
		var message Message
		if json.Unmarshal([]byte(fmtSyncString(row["body"])), &message) != nil || message.SessionID != session || message.DocumentID != libraryChatOwner || message.ID != row["id"] || message.Role != "user" && message.Role != "assistant" {
			return errors.New("云端书库聊天记录无效")
		}
	case "library_chat_runs":
		var run ChatRun
		if row["session_id"] != session || json.Unmarshal([]byte(fmtSyncString(row["body"])), &run) != nil || run.DocumentID != libraryChatOwner || run.SessionID != session || run.ID != row["id"] {
			return errors.New("云端书库聊天状态无效")
		}
	}
	return nil
}
