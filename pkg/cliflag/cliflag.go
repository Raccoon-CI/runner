// Package cliflag holds the small flag types the commands share.
package cliflag

import "strings"

// List is a flag that can be given many times: -secret A -secret B.
type List []string

// String implements flag.Value.
func (l *List) String() string { return strings.Join(*l, ",") }

// Set implements flag.Value.
func (l *List) Set(s string) error { *l = append(*l, s); return nil }
