package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The intent survives a crash between exchange acceptance and book persistence.
// Only the runner, after saving its ledger, may clear it.
func recordIntent(path, clientID, symbol string, side Side, quantity float64) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("存在未核对订单日志 %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	intent := struct {
		ID        string    `json:"client_order_id"`
		Symbol    string    `json:"symbol"`
		Side      Side      `json:"side"`
		Quantity  float64   `json:"quantity"`
		CreatedAt time.Time `json:"created_at"`
	}{clientID, symbol, side, quantity, time.Now().UTC()}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func ClearIntent(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
