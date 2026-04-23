package agent

// splitArgs splits a string into arguments, respecting single and double
// quotes (shell-style).  Unquoted tokens are split on whitespace.  Quotes
// are consumed but not included in the resulting argument values.
//
//	splitArgs(`--name='hello world' --port=8080`)
//	=> ["--name=hello world", "--port=8080"]
func splitArgs(s string) []string {
	var args []string
	var cur []byte
	inSingle := false
	inDouble := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if len(cur) > 0 {
				args = append(args, string(cur))
				cur = cur[:0]
			}
		default:
			cur = append(cur, c)
		}
	}
	if len(cur) > 0 {
		args = append(args, string(cur))
	}
	return args
}
