package neoread

import (
	"encoding/json"
	"errors"
	"os"
)

type webdavConnection struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

// The desktop shares the connection across libraries; their enablement,
// folders, intervals and replication state stay in each library directory.
func (s *Server) SetWebDAVAccountPath(name string) error {
	s.webdav.accountPath = name
	if _, err := os.Stat(name); os.IsNotExist(err) {
		config, err := s.readSyncConfig()
		if err != nil {
			return err
		}
		if config.Account != "" && config.Password != "" {
			return s.saveSyncConnection(config)
		}
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) useSyncConnection(config webdavConfig) (webdavConfig, error) {
	if s.webdav.accountPath == "" {
		return config, nil
	}
	body, err := os.ReadFile(s.webdav.accountPath)
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return config, errors.New("无法读取 WebDAV 连接设置")
	}
	var connection webdavConnection
	if decodeSyncJSON(body, &connection) != nil {
		return config, errors.New("WebDAV 连接设置无效，请重新配置")
	}
	if config.Account == "" || config.Account == connection.Account {
		config.Account, config.Password = connection.Account, connection.Password
	}
	return config, nil
}

func (s *Server) saveSyncConnection(config webdavConfig) error {
	if s.webdav.accountPath == "" {
		return nil
	}
	body, err := json.Marshal(webdavConnection{Account: config.Account, Password: config.Password})
	if err != nil {
		return err
	}
	return atomicFile(s.webdav.accountPath, body)
}
