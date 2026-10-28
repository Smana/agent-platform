// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/config"
)

// M5: a room's repository comes from its Room; a Room not in the cache is "" (held), not an error.
func TestRoomRepository(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"}, Spec: v1alpha1.RoomSpec{Repository: "Smana/ai-native-ref"}}
	repo := roomRepository(fake.NewClientBuilder().WithScheme(scheme).WithObjects(room).Build(), "agent-system")
	if got, err := repo(t.Context(), "3kq7x2ma"); err != nil || got != "Smana/ai-native-ref" {
		t.Fatalf("repository %q, %v", got, err)
	}
	if got, err := repo(t.Context(), "aaaaaaaa"); err != nil || got != "" {
		t.Fatalf("a missing Room: %q, %v", got, err)
	}
}

func TestWebhookSubscribers(t *testing.T) {
	got := webhookSubscribers([]config.WebhookConfig{{Name: "slack-app", Network: "cluster", URL: "http://x", SecretFile: "/k",
		Events: []string{"room.ended"}, Repositories: []string{"a/b"}, Generation: 2}})
	if len(got) != 1 || got[0].Name != "slack-app" || got[0].Generation != 2 || got[0].Events[0] != "room.ended" || got[0].Repositories[0] != "a/b" {
		t.Fatalf("%+v", got)
	}
}
