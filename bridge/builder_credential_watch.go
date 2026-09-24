package bridge

import (
	"context"
	"slices"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// watchCredentials binds each credentials_uri to its target: every session
// built, and every receiver and sender rt holds. A declared receiver or sender
// no route uses is built but never handed to rt, so no Retire names it to the
// refresher's Forget: watched, it would keep its poller running after every
// reload that replaces its unit, until rt stops.
func watchCredentials(ctx context.Context, refresher *CredentialRefresher, rt *runtime.Runtime,
	sessions map[string]ports.Session, receivers map[string]ports.Receiver, senders map[string]ports.Sender,
	sessionURIs, receiverURIs, senderURIs map[string]string,
) {
	held := rt.CredentialTargets()
	holds := func(target any) bool {
		return slices.ContainsFunc(held, func(h any) bool { return sameTarget(h, target) })
	}
	for sid, uri := range sessionURIs {
		if sess, ok := sessions[sid]; ok {
			refresher.Watch(ctx, uri, sess)
		}
	}
	for rid, uri := range receiverURIs {
		if recv, ok := receivers[rid]; ok && holds(recv) {
			refresher.WatchReceiver(ctx, uri, recv)
		}
	}
	for sid, uri := range senderURIs {
		if snd, ok := senders[sid]; ok && holds(snd) {
			refresher.WatchSender(ctx, uri, snd)
		}
	}
}
