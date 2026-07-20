package store

import "encoding/hex"

// validGeneratedID accepts only the fixed representation returned by model.NewID.
// Keeping this check at the Store boundary prevents arbitrary caller text from becoming
// a task identifier, URL path value, foreign key, report field or diagnostic label.
func validGeneratedID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
