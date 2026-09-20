package mobileconfig

import (
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// A minimal property list writer. Only the handful of types a configuration
// profile needs are supported, which keeps this dependency-free and small
// enough to audit, worthwhile for a file that carries a CA certificate and,
// optionally, a password.

func writeValue(w io.Writer, v any, depth int) error {
	pad := strings.Repeat("\t", depth)
	switch val := v.(type) {
	case dict:
		if _, err := fmt.Fprintf(w, "%s<dict>\n", pad); err != nil {
			return err
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		// Sorted so the output is byte-identical between runs, which makes a
		// regenerated profile diffable.
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "%s\t<key>%s</key>\n", pad, escape(k)); err != nil {
				return err
			}
			if err := writeValue(w, val[k], depth+1); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "%s</dict>", pad)
		return err

	case []dict:
		if _, err := fmt.Fprintf(w, "%s<array>\n", pad); err != nil {
			return err
		}
		for _, item := range val {
			if err := writeValue(w, item, depth+1); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "%s</array>", pad)
		return err

	case string:
		_, err := fmt.Fprintf(w, "%s<string>%s</string>", pad, escape(val))
		return err

	case int:
		_, err := fmt.Fprintf(w, "%s<integer>%s</integer>", pad, strconv.Itoa(val))
		return err

	case bool:
		tag := "false"
		if val {
			tag = "true"
		}
		_, err := fmt.Fprintf(w, "%s<%s/>", pad, tag)
		return err

	case data:
		encoded := base64.StdEncoding.EncodeToString(val)
		if _, err := fmt.Fprintf(w, "%s<data>\n", pad); err != nil {
			return err
		}
		// Apple's tooling expects wrapped base64.
		for i := 0; i < len(encoded); i += 60 {
			end := min(i+60, len(encoded))
			if _, err := fmt.Fprintf(w, "%s%s\n", pad, encoded[i:end]); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "%s</data>", pad)
		return err
	}
	return fmt.Errorf("mobileconfig: cannot encode %T", v)
}

var escaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func escape(s string) string { return escaper.Replace(s) }
