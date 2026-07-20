// Package logsafe provides log fields that cannot include an error's message.
//
// Network, URL, database and proxy libraries frequently embed endpoints, query
// tokens, local paths or credentials in Error(). Public deployments must keep the
// operation and correlation IDs for diagnosis without serializing that text.
package logsafe

import "fmt"

// ErrorType returns only the concrete Go error type. The returned value describes
// the failing subsystem while deliberately excluding every byte from Error().
func ErrorType(err error) string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", err)
}
