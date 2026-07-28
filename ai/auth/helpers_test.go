package auth

import (
	"fmt"
	"strconv"
)

func fmtItoa(i int) string { return strconv.Itoa(i) }

func fmtSscan(s string, out *int) (int, error) { return fmt.Sscanf(s, "%d", out) }
