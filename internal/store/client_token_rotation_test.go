package store

import "testing"

func TestRotateClientTokenPreservesIdentityAndInvalidatesOldToken(t *testing.T) {
	database, err := Open(t.TempDir() + "/rotation.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	created, oldToken, err := database.CreateClient(t.Context(), "migration-node", map[string]string{"region": "hk"})
	if err != nil {
		t.Fatal(err)
	}
	newToken, err := database.RotateClientToken(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newToken == "" || newToken == oldToken {
		t.Fatalf("rotated token was not replaced")
	}
	if _, err := database.AuthenticateClient(t.Context(), oldToken); err == nil {
		t.Fatal("old token still authenticates after rotation")
	}
	authenticated, err := database.AuthenticateClient(t.Context(), newToken)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != created.ID || authenticated.Name != created.Name {
		t.Fatalf("rotated credential changed Client identity: %+v", authenticated)
	}
}
