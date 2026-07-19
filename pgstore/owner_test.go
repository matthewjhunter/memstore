package pgstore

import (
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// ownerFor is the single decision about which user_id a written fact carries.
// It is pure, so it is tested directly rather than through a live database.
func TestOwnerFor(t *testing.T) {
	tests := []struct {
		name      string
		storeUser int64
		factUser  int64
		want      int64
		wantErr   string
	}{
		{
			// The overwhelmingly common case: a request-scoped store writing a
			// fact that says nothing about ownership.
			name:      "scoped store, fact silent about owner",
			storeUser: 7,
			factUser:  0,
			want:      7,
		},
		{
			name:      "scoped store, fact agrees with the store",
			storeUser: 7,
			factUser:  7,
			want:      7,
		},
		{
			// The case this function exists for. Insert used to prefer
			// f.UserID whenever it was set, so a handler that decoded a
			// request body straight into memstore.Fact would hand any caller a
			// cross-user write.
			name:      "scoped store cannot write another user's fact",
			storeUser: 7,
			factUser:  9,
			wantErr:   "scoped to user 7",
		},
		{
			// Service scope is the privileged daemon-internal scope. It may
			// write for any user, but must say which.
			name:      "service scope honours an explicit owner",
			storeUser: 0,
			factUser:  9,
			want:      9,
		},
		{
			// user_id is NOT NULL with an FK, so this would fail in the
			// database anyway. Failing here makes the reason legible.
			name:      "service scope requires an explicit owner",
			storeUser: 0,
			factUser:  0,
			wantErr:   "explicit Fact.UserID",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &PostgresStore{userID: tc.storeUser}
			got, err := s.ownerFor(memstore.Fact{UserID: tc.factUser})

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ownerFor() = %d, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ownerFor() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("ownerFor() = %d, want %d", got, tc.want)
			}
		})
	}
}
