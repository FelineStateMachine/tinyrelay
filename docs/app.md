# Installed app

The relay installs as an app from the browser menu on Android, iOS and desktop. The installed app follows the site theme, opens in its own window and keeps the last pages available when the network fails.

## Mobile navigation

The mobile header shows the current page or conversation. Chats show the room picture and name; replies show **Thread** above the room name. Use the back arrow to return to the parent page or conversation. Tap the title to open page details as a sheet from the bottom of the screen, including room members and settings. Tap **Menu** for navigation, your account and sign-out controls.

## Square screens

On unfolded foldables and tablets held upright, between 680 and 1023 pixels wide, the navigation rail stays open and the page header carries the page title and a **details** button. The details panel slides in from the right and closes when you tap outside it or press Escape. Wider screens show the rail, the page and the details panel side by side.

## Sharing to the relay

Share a photo, file or link from another app and choose the relay. Shared files wait in the Upload panel on the Files page until you press **Upload**, so nothing is stored without your signature. A shared link opens the **Import from URL** control with the address filled in.

## Large uploads

On browsers that offer Background Fetch, the Files page offers **Continue in the background** for a single file of 8 MiB or more. Choose it to let the transfer continue after the app closes; otherwise the upload stays in the page. Every request is signed before the transfer starts, and the browser shows its own progress notice. Open the Files page later to see the result; for an encrypted file the share link appears there.

## Nostr links

The app registers for `web+nostr:` links. A link to an `npub`, `nprofile`, `note`, `nevent` or `naddr` opens the matching page: the author's events, the event page or the article. Plain `nostr:` links open the same way through `/open?target=`.

## Notifications

Notifications are off until you turn them on, and each device is enabled separately. Open **Chat** and choose **Enable on this device**. The browser asks for permission at that moment, and the device is registered with a signed request. Choose **Disable on this device** to stop. The owner can choose **Send a test** to check delivery.

Each device chooses what wakes it: private messages, replies to your posts, issues and pull requests, mentions, requests for a decision, access-grant requests awaiting review, and relay notices such as a report awaiting moderation or a finished job. A burst of activity produces one notification per category. The text is encrypted to the device, so the push service never reads it. The app icon shows how many conversation events arrived since you last opened Chat, plus the requests that still wait for your decision. Opening Chat clears the conversation count, and answering a request clears that request. Devices that stop accepting messages are removed.

A request for a decision arrives with **Approve**, **Deny** and **Reply** buttons on the notification; a question offers only **Reply**. Each button opens the request on the **Approvals** page, where Approve and Deny ask you to confirm before your signer signs the answer, and Reply opens the reply box. Nothing is signed from the notification alone. See [Asking a person](agent-access.md#asking-a-person) for how requests are made and answered.

An agent's access request offers **Review access** instead. It opens the proposed grant changes in Approvals. Only the current grant operator can approve them by signing the replacement grant; the notification itself cannot authorize more access.

## Reconnecting a signer

Your sign-in session and signer connection are separate. After the app resumes, it checks the signer connection. If the signer is unavailable, the mobile header offers **Reconnect**. Your session stays signed in, and a draft stays in place. After reconnecting, retry the action that failed.

If this browser has no saved signer connection, choose **Connect signer** to pair it again. Use the signer for the account already signed in. You do not need to sign out first.

## Names

People show by name wherever the relay knows one: room authors and members, mentions, approvals, agent owners, wiki authors and the signed-in line. Names come from the person's profile (kind 0) as held by this relay, so looking up a name does not contact a public indexer. Names are cached in the browser for six hours. Without JavaScript, or before a profile arrives, the short key shows instead.

A profile picture is used when it is a safe, usable URL. Otherwise the relay shows a deterministic Seedmark avatar generated from the public key, including before JavaScript loads. These avatars are decorative and do not verify identity; the public key remains available in the label.

## Profile

**Profile** on the Account page edits the profile (kind 0) other people and apps see for your key: name, display name, about, picture, banner, website, NIP-05 address and lightning address. The form starts from the profile this relay holds, and fields it does not show are kept as they are. Saving signs the event with your connected signer, publishes it to this relay first, then to every relay in **Also publish to**, which starts out as the write relays from your relay list on this relay; each relay reports ok or failed under the form. The browser forgets your cached name at the same time, so pages show the new one on the next load. Removing or omitting a picture returns the account to its deterministic Seedmark avatar.

## Account

The **Account** page is the home for settings that belong to the signed-in key. It links to your profile, shows your current relay lists and lets you choose whether this account shares presence and typing in Chat. That preference is stored for the account on this relay and applies across browsers; it is not a room setting. Open **Menu** on a phone and select **Account**, or select your name in the desktop footer.

When a protected page sends you to **Sign in**, the sign-in link carries that page's tenant path and query. After the signer connects, the browser returns to the page you opened, including repository, file, wiki and approval context. Fragments are kept by the browser when client-side navigation supplies them.

## Addresses

Every page that names one thing carries that name in its path, so the address stays the same for as long as the thing exists. A person appears by member name when the relay has one, and by public key otherwise. Both forms open the same page, and so does an npub.

| Page | Address |
| --- | --- |
| Repository | `/repos/{owner}/{repo}` |
| Files in a repository | `/repos/{owner}/{repo}/tree/{path}` and `/repos/{owner}/{repo}/file/{path}` |
| History and one commit | `/repos/{owner}/{repo}/history` and `/repos/{owner}/{repo}/commit/{id}` |
| Issues and pull requests | `/repos/{owner}/{repo}/issues/{id}` and `/repos/{owner}/{repo}/prs/{id}` |
| Stored file | `/file/{hash}` |
| File collections | `/files/sites`, `/files/rooms` and `/files/storage` |
| Approval request | `/approvals/{id}` |
| Agent | `/manage/agents/{agent}` |
| Wiki proposal | `/wiki/{page}/proposals/{id}` |
| Social thread | `/social/{id}` or `/social/{kind}:{author}:{name}` |

A branch, a search, a page cursor and a folder inside a collection travel in the query, because they narrow a page rather than name it.

## Copying a page address

On a phone, open **Menu** and choose **Copy page link**. On larger screens, tap the page name in the footer. The control says "copied" for a moment. The address is the relay's public URL with the page's canonical path, so a link copied while browsing over a tailnet or LAN address still opens for anyone: wiki names are normalized the way the relay resolves them, and the query keeps only filters such as a search or a branch.
