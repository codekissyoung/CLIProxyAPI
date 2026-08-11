package auth

import "testing"

type invalidParamStatusError struct {
	code int
	msg  string
}

func (e invalidParamStatusError) Error() string   { return e.msg }
func (e invalidParamStatusError) StatusCode() int { return e.code }

// HTTP 400 request failures from the Codex upstream must be classified as
// request-invalid so the conductor neither fans the request out across every
// credential nor marks the account unhealthy.
func TestIsRequestInvalidErrorUnsupportedParameter(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "codex unsupported parameter detail",
			err:  invalidParamStatusError{code: 400, msg: `{"detail":"Unsupported parameter: reasoning_effort"}`},
			want: true,
		},
		{
			name: "codex unknown parameter detail",
			err:  invalidParamStatusError{code: 400, msg: `{"detail":"Unknown parameter: 'input[4].status'"}`},
			want: true,
		},
		{
			name: "lowercase variant",
			err:  invalidParamStatusError{code: 400, msg: `unsupported parameter: foo`},
			want: true,
		},
		{
			name: "generic 400 remains request-scoped",
			err:  invalidParamStatusError{code: 400, msg: `{"detail":"something else"}`},
			want: true,
		},
		{
			name: "401 stays auth-scoped, not request-invalid",
			err:  invalidParamStatusError{code: 401, msg: `{"detail":"Unsupported parameter: reasoning_effort"}`},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRequestInvalidError(tc.err); got != tc.want {
				t.Errorf("isRequestInvalidError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
