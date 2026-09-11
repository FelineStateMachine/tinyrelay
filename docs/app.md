# Installed app

The relay installs as an app from the browser menu on Android, iOS and desktop. The installed app follows the site theme, opens in its own window and keeps the last pages available when the network fails.

## Sharing to the relay

Share a photo, file or link from another app and choose the relay. Shared files wait in the Upload panel on the Files page until you press **Upload**, so nothing is stored without your signature. A shared link opens the **Import from URL** control with the address filled in.

## Large uploads

On browsers that offer it, a file of 8 MiB or more uploads through the browser's background transfer, so the upload continues after the app closes. Every request is signed before the transfer starts, and the browser shows its own progress notice. Open the Files page later to see the result; for an encrypted file the share link appears there. On other browsers the upload runs while the page stays open.

## Nostr links

The app registers for `web+nostr:` links. A link to an `npub`, `nprofile`, `note`, `nevent` or `naddr` opens the matching page: the author's events, the event page or the article. Plain `nostr:` links open the same way through `/open?target=`.

## Notifications

Notifications are off until you turn them on, and each device is enabled separately. Open **Chat** and choose **Enable on this device**. The browser asks for permission at that moment, and the device is registered with a signed request. Choose **Disable on this device** to stop. The owner can choose **Send a test** to check delivery.

Each device chooses what wakes it: private messages, replies to your posts, issues and pull requests, mentions, requests for a decision, and relay notices such as a report awaiting moderation or a finished job. A burst of activity produces one notification per category. The text is encrypted to the device, so the push service never reads it. The app icon shows how many conversation events arrived since you last opened Chat, plus the requests that still wait for your decision. Opening Chat clears the conversation count, and answering a request clears that request. Devices that stop accepting messages are removed.

A request for a decision arrives with **Approve**, **Deny** and **Reply** buttons on the notification; a question offers only **Reply**. Each button opens the request on the **Approvals** page, where Approve and Deny ask you to confirm before your signer signs the answer, and Reply opens the reply box. Nothing is signed from the notification alone. See [Asking a person](agents.md#asking-a-person) for how requests are made and answered.

## Names

People show by name wherever the relay knows one: room authors and members, mentions, approvals, agent owners, wiki authors and the signed-in line. The name is the profile (kind 0) published on this relay, resolved in the browser by the `nostr-name` element from [nostr-web-components](https://web.nostr.technology/), which tiny vendors as one bundled script. The bundle replaces the library's profile loader with one that asks this relay only, so no public indexer learns who you look at; results are cached in the browser for six hours. Without JavaScript, or before a profile arrives, the short key shows instead, and the key stays in the element's title.

Rebuild the bundle after changing `scripts/nostr-name-setup.mjs` or updating the package with `npm run build:nostr-name`. The package pulls its dependencies from the JSR registry, which `.npmrc` maps for the `@jsr` scope.

## Profile

**Profile** in the home page's Actions, shown once you are signed in, edits the profile (kind 0) other people and apps see for your key: name, display name, about, picture, banner, website, NIP-05 address and lightning address. The form starts from the profile this relay holds, and fields it does not show are kept as they are. Saving signs the event with your connected signer, publishes it to this relay first, then to every relay in **Also publish to**, which starts out as the write relays from your relay list on this relay; each relay reports ok or failed under the form. The browser forgets your cached name at the same time, so pages show the new one on the next load.



## Copying a page address

The crumb that names the current page, next to the relay name at the top on phones and in the footer on larger screens, copies the page's address when tapped and says "copied" for a moment. The address is the relay's public URL with the page's canonical path, so a link copied while browsing over a tailnet or LAN address still opens for anyone: wiki names are normalized the way the relay resolves them, and repositories and files keep the query that names them.
\n
