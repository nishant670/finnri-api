# Push notification delivery — deferred

**Status:** not started. Deferred deliberately on 2026-08-18; everything below is
scoped but unbuilt.

## The problem

`createNotification` in `finnri-api/internal/http/notifications.go` writes a row
in `notifications` and stops. Nothing sends it anywhere. The only code in the
repo that talks to Expo's push service is
`finnri-api/internal/http/subscription_automation.go`, and it does so on its own
rather than through a shared sender.

So every notification the product raises — split group invites, invite
acceptances, budget alerts — is **in-app only**. It exists once the user opens
Finnri and looks. Nothing reaches a locked phone.

For split group invites this is partly mitigated: an in-app prompt now surfaces a
pending invite the moment the invitee opens the app, with Accept / Check later
(see `finnri-app/components/split/SplitInvitePrompt.tsx`). That closes the "she
never finds out" gap for anyone who opens the app, but not for anyone who
doesn't.

## What it takes

1. **Extract a shared sender.** Pull the Expo push call out of
   `subscription_automation.go` into something like
   `internal/push/expo.go` — `Send(userID, title, body, data)` that looks up the
   user's rows in `push_devices` and posts to Expo, with the receipt handling and
   token-invalidation the automation path already works out.
2. **Wire it into `createNotification`.** Every notification then has one path to
   the device. Keep the DB write authoritative and the push best-effort: a failed
   send must never fail the request that raised it.
3. **Respect per-type preferences.** There is no notification-preference model
   today. Decide whether one is needed before turning on pushes for every type,
   or the first noisy category will train people to disable the lot.
4. **FCM credentials on EAS.** Android delivery needs them uploaded; see
   `[[finnri-friends-build-blockers]]`. Without this step nothing above is
   testable on a real device.
5. **Deep links.** Notifications already carry `action_url` (e.g.
   `/invite/split/<token>`). Confirm the push payload carries it and that
   `finnri-app/app/+native-intent.ts` routes a cold-start tap to the right screen.

## Order

4 gates any real testing, so do it first. Then 1 → 2, with 3 decided before 2
ships to anyone who is not a tester. 5 is small and can ride along with 2.
