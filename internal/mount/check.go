package mount

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/deppess/sfync/internal/config"
)

// IsReachable checks if the remote server is accessible via TCP.
func IsReachable(profile *config.Profile) error {
	addr := net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", addr, err)
	}
	conn.Close()
	return nil
}
