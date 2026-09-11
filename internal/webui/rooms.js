// Room custom elements and rendering/live-update helpers. This feature module
// runs after components.js has published the shared UI helper contract.
(() => {
  "use strict";
  // Room elements own composition, room management actions and live updates.
  // room-compose signs kind 9 and 12 events, room-create signs room metadata,
  // room-action handles membership controls, and room-live consumes the room
  // event stream.
  const tiny = window.tiny;
  const {FormElement, el, isHex64} = tiny.ui;
 // Rooms: the compose bar, room creation, room administration and the live
 // stream. Each signed event is built here, signed by the connected signer,
 // verified unchanged and published once to /events. room-message markup
 // matches the roomMessage template so streamed messages read the same.
 const roomKinds = [9, 11, 12, 40002, 44100, 44101];
 const mentionPattern = /(^|[\s(])@(npub1[02-9ac-hj-np-z]{58}|[0-9a-f]{64})\b/g;
 const roomLinkPattern = /https?:\/\/[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+/g;
 const now = () => Math.floor(Date.now() / 1000);
 // keyHex accepts a hex key or an npub and returns the hex key, or null.
 const keyHex = value => {
   const text = String(value || "").trim().replace(/^nostr:/, "");
   if (isHex64(text)) return text;
   if (!/^npub1[02-9ac-hj-np-z]{58}$/.test(text)) return null;
   try { const decoded = window.NostrSigner?.decodeNpub?.(text); return isHex64(decoded) ? decoded : null; } catch { return null; }
 };
 // roomMentions lists the keys named as @npub or @hex in a message, once each.
 const roomMentions = text => {
   const keys = [];
   for (const match of String(text).matchAll(mentionPattern)) {
     const key = keyHex(match[2]);
     if (key && !keys.includes(key)) keys.push(key);
   }
   return keys;
 };
 // roomID derives a room id from a name: lowercase letters, digits, hyphen and underscore.
 const roomID = name => String(name || "").toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 64);
 const tagValue = (event, name) => (event.tags || []).find(tag => tag[0] === name)?.[1] || "";
 const quoteID = event => { const id = tagValue(event, "q"); return isHex64(id) ? id : ""; };
 const clock = seconds => {
   const stamp = new Date(seconds * 1000).toISOString();
   if (stamp.slice(0, 10) === new Date().toISOString().slice(0, 10)) return stamp.slice(11, 16);
   return ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][Number(stamp.slice(5, 7)) - 1] + " " + Number(stamp.slice(8, 10)) + " " + stamp.slice(11, 16);
 };
 const roomPath = (room, rest = "") => tiny.localPath("/rooms/" + encodeURIComponent(room) + rest);
 // linkify fills a node with escaped text, turning http(s) URLs and nostr
 // links into anchors; nostr links open through /open.
 const linkify = (node, text) => {
   let last = 0;
   for (const match of String(text).matchAll(roomLinkPattern)) {
     node.append(text.slice(last, match.index));
     const trimmed = match[0].replace(/[.,;:!?)]+$/, "");
     const link = el("a", trimmed);
     link.href = trimmed.startsWith("http") ? trimmed : tiny.localPath("/open?target=" + encodeURIComponent(trimmed));
     if (trimmed.startsWith("http")) link.rel = "noopener";
     node.append(link, match[0].slice(trimmed.length));
     last = match.index + match[0].length;
   }
   node.append(text.slice(last));
   return node;
 };
 // chatMarkdown renders the same Markdown subset the page renders for chat
 // messages: paragraphs with line breaks, fenced code, headings, lists,
 // inline code, emphasis, links, and bare http(s) or nostr: references. It
 // builds nodes rather than markup, so message text never becomes HTML.
 const inlinePattern = /(`[^`]+`)|\[([^\]]+)\]\(([^)\s]+)\)|\*\*([^*]+)\*\*|(^|[^*\w])\*([^*]+)\*|(https?:\/\/[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+)/g;
 const safeLink = target => /^(https?:\/\/|\/(?!\/)|\.\.?\/|#|mailto:)/.test(target) || (!target.includes(":") && !target.startsWith("//"));
 const chatInline = (node, text) => {
   let last = 0;
   for (const match of String(text).matchAll(inlinePattern)) {
     node.append(text.slice(last, match.index));
     last = match.index + match[0].length;
     if (match[1]) { node.append(el("code", match[1].slice(1, -1))); continue; }
     if (match[2] !== undefined) {
       if (!safeLink(match[3])) { node.append(match[0]); continue; }
       const link = el("a", match[2]);
       link.href = match[3];
       if (match[3].startsWith("http")) link.rel = "noopener";
       node.append(link);
       continue;
     }
     if (match[4] !== undefined) { node.append(chatInline(el("strong"), match[4])); continue; }
     if (match[6] !== undefined) { node.append(match[5], chatInline(el("em"), match[6])); continue; }
     const trimmed = match[7].replace(/[.,;:!?)]+$/, "");
     const link = el("a", trimmed);
     link.href = trimmed.startsWith("http") ? trimmed : tiny.localPath("/open?target=" + encodeURIComponent(trimmed));
     if (trimmed.startsWith("http")) link.rel = "noopener";
     node.append(link, match[7].slice(trimmed.length));
   }
   node.append(text.slice(last));
   return node;
 };
 const pipeEscaped = (value, index) => {
   let slashes = 0;
   for (let i = index - 1; i >= 0 && value[i] === "\\"; i--) slashes++;
   return slashes % 2 === 1;
 };
 const pipeRow = line => {
   let value = String(line).trim();
   if (!value.includes("|")) return null;
   const leadingPipe = value.startsWith("|");
   const trailingPipe = value.endsWith("|") && !pipeEscaped(value, value.length - 1);
   if (value.startsWith("|")) value = value.slice(1);
   const last = value.length - 1;
   if (value.endsWith("|") && !pipeEscaped(value, last)) value = value.slice(0, -1);
   const cells = [];
   let hasPipe = false;
   let cell = "", code = false;
   for (let i = 0; i < value.length; i++) {
     const character = value[i];
     if (character === "`") { code = !code; cell += character; continue; }
     if (character === "|" && pipeEscaped(value, i)) { cell = cell.slice(0, -1) + "|"; continue; }
     if (character === "|" && !code) {
       hasPipe = true; cells.push(cell.trim()); cell = ""; continue;
     }
     cell += character;
   }
   cells.push(cell.trim());
   return (hasPipe || leadingPipe || trailingPipe) ? cells : null;
 };
 const tableDelimiter = cell => {
   const value = String(cell).trim();
   if (!/^:?-{3,}:?$/.test(value)) return null;
   return value.startsWith(":") ? (value.endsWith(":") ? "center" : "left") : (value.endsWith(":") ? "right" : "");
 };
 const tableBodyLine = line => {
   const value = String(line).trim();
   return Boolean(value) && !value.startsWith("#") && !value.startsWith("```") && !value.startsWith("~~~");
 };
 const markdownTable = (lines, start) => {
   const header = pipeRow(lines[start]), separator = pipeRow(lines[start + 1]);
   if (!header || !separator || header.length !== separator.length) return null;
   const align = separator.map(tableDelimiter);
   if (align.some(value => value === null)) return null;
   const rows = [];
   let index = start + 2;
   for (; index < lines.length; index++) {
     if (!tableBodyLine(lines[index])) break;
     const row = pipeRow(lines[index]);
     if (!row) break;
     rows.push(row);
   }
   const wrapper = el("div");
   wrapper.dataset.markdownTable = "";
   wrapper.setAttribute("data-markdown-table", "");
   wrapper.setAttribute("role", "region");
   wrapper.setAttribute("aria-label", "Table");
   wrapper.setAttribute("tabindex", "0");
   const table = el("table"), thead = el("thead"), heading = el("tr");
   header.forEach((cell, column) => {
     const th = el("th");
     th.setAttribute("scope", "col");
     if (align[column]) { th.dataset.align = align[column]; th.setAttribute("data-align", align[column]); }
     chatInline(th, cell);
     heading.append(th);
   });
   thead.append(heading);
   const tbody = el("tbody");
   rows.forEach(row => {
     const tr = el("tr");
     for (let column = 0; column < header.length; column++) {
       const td = el("td");
       if (align[column]) { td.dataset.align = align[column]; td.setAttribute("data-align", align[column]); }
       chatInline(td, row[column] || "");
       tr.append(td);
     }
     tbody.append(tr);
   });
   table.append(thead, tbody);
   wrapper.append(table);
   return {node: wrapper, end: index};
 };
 const listItem = line => /^\s*(?:[-*+]|\d+[.)])\s+/.test(line);
 const chatMarkdown = (node, text) => {
   const lines = String(text).replace(/\r\n/g, "\n").split("\n");
   let paragraph = [];
   const flush = () => {
     if (!paragraph.length) return;
     const p = el("p");
     paragraph.forEach((line, index) => { if (index) p.append(el("br")); chatInline(p, line); });
     node.append(p);
     paragraph = [];
   };
   for (let i = 0; i < lines.length; i++) {
     const line = lines[i];
     const fence = line.trim().match(/^(`{3,}|~{3,})/);
     const table = !fence && markdownTable(lines, i);
     if (fence) {
       flush();
       const code = [], marker = fence[1];
       const closesFence = value => value.length >= marker.length && [...value].every(character => character === marker[0]);
       for (i++; i < lines.length && !closesFence(lines[i].trim()); i++) code.push(lines[i]);
       const pre = el("pre"); pre.append(el("code", code.join("\n"))); node.append(pre);
     } else if (table) {
       flush();
       node.append(table.node);
       i = table.end - 1;
     } else if (line.startsWith("#")) {
       flush();
       const level = Math.min(6, line.length - line.replace(/^#+/, "").length);
       node.append(chatInline(el("h" + level), line.slice(level).trim()));
     } else if (listItem(line)) {
       flush();
       const list = el(/^\s*\d/.test(line) ? "ol" : "ul");
       for (; i < lines.length && listItem(lines[i]); i++) list.append(chatInline(el("li"), lines[i].replace(/^\s*(?:[-*+]|\d+[.)])\s+/, "")));
       i--;
       node.append(list);
     } else if (!line.trim()) {
       flush();
     } else {
       paragraph.push(line.trim());
     }
   }
   flush();
   return node;
 };
 // NIP-92 references are matched outside code. Only standalone attachment
 // lines are removed; prose links and examples keep their original text.
 const attachmentURLPattern = /^https?:\/\/[^\s\x00-\x1f\x7f\\<>"']+$/;
 const attachmentReference = /!?\[((?:\\.|[^\]\\])*)\]\(([^)\s]+)\)|(https?:\/\/[^\s<>"']+)/g;
 const attachmentLines = (text, visit) => {
   let marker = "";
   return String(text || "").replace(/\r\n/g, "\n").split("\n").map(line => {
     const trimmed = line.trim();
     if (marker) {
       if (trimmed.length >= marker.length && [...trimmed].every(c => c === marker[0])) marker = "";
       return line;
     }
     const fence = trimmed.match(/^(`{3,}|~{3,})/);
     if (fence) { marker = fence[1]; return line; }
     return visit(line);
   }).join("\n");
 };
 const attachmentOutsideCode = line => {
   let out = "";
   while (line.length) {
     const start = line.indexOf("`");
     if (start < 0) return out + line;
     out += line.slice(0, start);
     line = line.slice(start);
     const delimiter = line.match(/^`+/)[0], end = line.indexOf(delimiter, delimiter.length);
     if (end < 0) return out + line;
     out += " ";
     line = line.slice(end + delimiter.length);
   }
   return out;
 };
 const attachmentReferenceURL = match => match[2] || match[3].replace(/[.,;:!?)]+$/, "");
 const attachmentURLVisible = (text, target) => {
   let visible = false;
   attachmentLines(text, line => {
     for (const match of attachmentOutsideCode(line).matchAll(attachmentReference)) if (attachmentReferenceURL(match) === target) visible = true;
     return line;
   });
   return visible;
 };
 const attachmentText = (text, limit) => [...String(text || "")].slice(0, limit).join("");
 const roomAttachments = event => {
   const out = [], seen = new Set(), content = String(event?.content || "");
   for (const tag of (event?.tags || [])) {
     if (!Array.isArray(tag) || tag[0] !== "imeta" || out.length >= 16) continue;
     const item = {};
     for (const field of tag.slice(1)) {
       if (typeof field !== "string") continue;
       const split = field.indexOf(" ");
       if (split < 1) continue;
       const key = field.slice(0, split), value = field.slice(split + 1);
       if (!value) continue;
       if (key === "url") item.url = value;
       else if (key === "m") item.mime = value.toLowerCase();
       else if (key === "x") item.hash = value;
       else if (key === "size") item.size = /^-?\d+$/.test(value) ? Number(value) : 0;
       else if (key === "filename" || key === "name") item.name = value;
       else if (key === "alt") item.alt = value;
     }
     if (!attachmentURLPattern.test(item.url || "") || seen.has(item.url) || !attachmentURLVisible(content, item.url)) continue;
     let parsed;
     try { parsed = new URL(item.url); } catch { continue; }
     if (!parsed.hostname || parsed.username || parsed.password) continue;
     item.size = Number.isSafeInteger(item.size) && item.size > 0 && item.size <= 1099511627776 ? item.size : 0;
     item.name = attachmentText(item.name, 255);
     item.alt = attachmentText(item.alt, 500);
     if (!item.name) item.name = item.alt;
     if (!item.name) { try { item.name = decodeURIComponent(parsed.pathname.split("/").pop() || "Attachment"); } catch { item.name = "Attachment"; } }
     seen.add(item.url);
     out.push(item);
   }
   return out;
 };
 const attachmentContent = (text, items) => {
   const urls = new Set(items.map(item => item.url));
   return attachmentLines(text, line => {
     const trimmed = line.trim(), match = [...trimmed.matchAll(attachmentReference)][0];
     return match && match[0] === trimmed && urls.has(attachmentReferenceURL(match)) ? "" : line;
   }).trim();
 };
 const attachmentSize = size => size < 1024 ? size + " B" : size < 1048576 ? Math.floor(size / 1024) + " KB" : Math.floor(size / 1048576) + " MB";
 const attachmentNodes = items => {
   if (!items.length) return null;
   const box = el("div"); box.dataset.roomAttachments = "";
   for (const item of items) {
     const figure = el("figure"), name = item.name || "Attachment";
     if (item.mime?.startsWith("image/") && item.mime !== "image/svg+xml") {
       const link = el("a"), image = el("img");
       link.href = item.url; link.rel = "noopener noreferrer"; link.setAttribute("fx-ignore", "");
       image.src = item.url; image.alt = item.alt || name; image.loading = "lazy"; image.referrerPolicy = "no-referrer";
       link.append(image); figure.append(link);
     } else if (item.mime?.startsWith("video/") || item.mime?.startsWith("audio/")) {
       const media = el(item.mime.startsWith("video/") ? "video" : "audio");
       media.controls = true; media.preload = "none"; media.src = item.url; figure.append(media);
     }
     const caption = el("figcaption"), link = el("a", name);
     link.href = item.url; link.download = name; link.dataset.download = ""; link.rel = "noopener noreferrer"; link.setAttribute("fx-ignore", "");
     caption.append(link);
     if (item.size > 0) caption.append(" ", el("small", attachmentSize(item.size)));
     figure.append(caption); box.append(figure);
   }
   return box;
 };
 const keyNode = hex => { const node = el("nostr-key", hex.slice(0, 12)); node.setAttribute("hex", hex); node.title = hex; return node; };
 // nameNode shows a person: the vendored nostr-name element replaces the short
 // id with the profile name published on this relay.
 const nameNode = hex => { const node = el("nostr-name", hex.slice(0, 12)); node.setAttribute("pubkey", hex); node.title = hex; return node; };
 // panelMembers reads the members list in the panel: role and agent marker by key.
 const panelMembers = () => {
   const members = {};
   document.querySelectorAll("#members li").forEach(item => {
     const hex = item.querySelector("nostr-name")?.getAttribute("pubkey") || item.querySelector("nostr-key")?.getAttribute("hex");
     if (hex) members[hex] = {role: (item.querySelector("small")?.textContent || "").split("|")[0].trim(), agent: item.hasAttribute("data-agent"), operator: item.getAttribute("data-operator") || ""};
   });
   return members;
 };
 const replyRoot = event => {
   if (![9, 12].includes(event?.kind)) return "";
   let root = "", reply = "", first = "", marked = false;
   for (const tag of event.tags || []) {
     if (tag[0] !== "e" || !isHex64(tag[1])) continue;
     const marker = tag[3] || "";
     marked ||= Boolean(marker);
     if (marker === "root") root = tag[1];
     else if (marker === "reply") reply = tag[1];
     else if (!marker && !first) first = tag[1];
   }
   return root || reply || (!marked ? first : "");
 };
 const messageNode = (event, {members = {}, room = "", inThread = false} = {}) => {
   const node = el("room-message");
   const notice = event.kind === 44100 ? "joined the room" : event.kind === 44101 ? "left the room" : "";
   const pubkey = notice ? tagValue(event, "p") : event.pubkey;
   const viewer = (document.getElementById("room") || document.getElementById("thread"))?.dataset.viewer;
   node.id = "msg-" + event.id;
   node.dataset.id = event.id;
   node.dataset.kind = String(event.kind);
   node.dataset.updatedAt = String(event.created_at);
   node.dataset.pubkey = pubkey;
   const member = members[pubkey];
   const own = !notice && Boolean(viewer) && pubkey === viewer;
   if (own) node.dataset.own = "";
   if (member?.agent) node.dataset.agent = "";
   if (notice) node.dataset.notice = "";
   const avatar = el("nostr-avatar", pubkey.slice(0, 2).toUpperCase());
   avatar.setAttribute("pubkey", pubkey);
   avatar.setAttribute("aria-hidden", "true");
   const header = el("header"), name = el("b"), time = el("time", clock(event.created_at)), small = el("small");
   name.append(nameNode(pubkey));
   time.dateTime = new Date(event.created_at * 1000).toISOString().replace(/\.\d+Z$/, "Z");
   time.title = time.dateTime.slice(0, 16).replace("T", " ") + " UTC";
   small.append(time);
   header.append(name);
   if (own) { const label = el("span", "you"); label.dataset.ownLabel = ""; header.append(label); }
   if (member?.agent) {
     header.append(" | ");
     if (member.operator) header.append(nameNode(member.operator), "'s agent");
     else header.append("agent");
   } else if (member?.role) header.append(" | " + member.role);
   header.append(small);
   const attachments = notice ? [] : roomAttachments(event);
   const body = notice ? el("p", notice) : chatMarkdown(el("div"), attachmentContent(event.content || "", attachments));
   node.append(avatar, header);
   const quote = notice ? "" : quoteID(event);
   if (quote) { const block = el("blockquote"), link = el("a", "quoted event " + quote.slice(0, 12)); block.dataset.quote = ""; link.href = tiny.localPath("/open?target=" + encodeURIComponent("nostr:" + quote)); block.title = quote; block.append(link); node.append(block); }
   node.append(body);
   const media = attachmentNodes(attachments); if (media) node.append(media);
   const footer = el("footer");
   const mentions = notice ? [] : (event.tags || []).filter(tag => tag[0] === "p" && isHex64(tag[1]) && tag[1] !== pubkey).map(tag => tag[1]);
   if (mentions.length) { const span = el("span", "to "); mentions.forEach(key => span.append(nameNode(key), " ")); footer.append(span); }
   if (!inThread && room && [9, 11, 40002].includes(event.kind)) { const link = el("a", "thread"); link.href = roomPath(room, "/thread/" + event.id); footer.append(link); }
   const root = replyRoot(event);
   if (!inThread && room && isHex64(root)) { const link = el("a", "in thread"); link.href = roomPath(room, "/thread/" + root); footer.append(link); }
   if (footer.childNodes.length) node.append(footer);
   return node;
 };
 // The timeline scrolls inside the content column on desktop and with the
 // page body on phones; roomBox finds whichever holds the overflow.
 const roomBox = () => [document.getElementById("content"), document.body, document.documentElement].find(node => node && node.scrollHeight > node.clientHeight + 1 && /auto|scroll/.test(getComputedStyle(node).overflowY)) || null;
 const roomNearBottom = () => {
   const box = roomBox();
   return !box || box.scrollHeight - box.scrollTop - box.clientHeight < 120;
 };
 const roomScroll = () => {
   const box = roomBox();
   if (box) box.scrollTop = box.scrollHeight;
 };
 // roomAppend adds one message to the timeline unless it is already there,
 // keeps the list bounded and follows the newest message when the viewer
 // is already reading the end of it.
 const roomAppend = (event, {room = "", inThread = false, own = false} = {}) => {
   const list = document.getElementById("messages");
   if (!list || !isHex64(event?.id) || document.getElementById("msg-" + event.id)) return null;
   list.roomSeen ||= new Set();
   if (list.roomSeen.has(event.id)) return null;
   list.roomSeen.add(event.id);
   while (list.roomSeen.size > 2000) list.roomSeen.delete(list.roomSeen.values().next().value);
   const reference = replyRoot(event);
   if (reference && !inThread) {
     const link = document.querySelector("#msg-" + reference + " > footer > a");
     if (link) { const count = Number((link.textContent.match(/(\d+) repl/) || [])[1] || 0) + 1; link.textContent = "thread | " + count + (count === 1 ? " reply" : " replies"); }
     return null;
   }
   const follow = own || roomNearBottom();
   list.querySelector("#empty")?.remove();
   const node = messageNode(event, {members: panelMembers(), room, inThread});
   list.append(node);
   while (list.children.length > 500) list.firstElementChild.remove();
   const root = tagValue(event, "e");
   if (event.kind === 12 && !inThread && isHex64(root)) {
     const link = document.querySelector("#msg-" + root + " > footer > a");
     if (link) { const count = Number((link.textContent.match(/(\d+) repl/) || [])[1] || 0) + 1; link.textContent = "thread | " + count + (count === 1 ? " reply" : " replies"); }
   }
   if (follow) roomScroll();
   return node;
 };
 // roomReact adds a reaction to its target's summary line.
 const roomReact = event => {
   const targets = (event.tags || []).filter(tag => tag[0] === "e");
   const target = targets.length && document.getElementById("msg-" + targets[targets.length - 1][1]);
   if (!target) return;
   let footer = target.querySelector(":scope > footer");
   if (!footer) { footer = el("footer"); target.append(footer); }
   const content = (event.content || "").trim() === "" || (event.content || "").trim() === "+" ? "+1" : event.content.trim();
   let span = [...footer.querySelectorAll("span[data-reaction]")].find(node => node.dataset.reaction === content);
   if (span) span.textContent = content + " " + (Number(span.textContent.slice(content.length)) + 1);
   else { span = el("span", content + " 1"); span.dataset.reaction = content; footer.append(span); }
 };
 // roomEdit applies a kind 40003 edit to the message it names when the
 // editor is that message's author, replacing the text and marking it.
 const roomEdit = event => {
   const targets = (event.tags || []).filter(tag => tag[0] === "e");
   const target = targets.length && document.getElementById("msg-" + targets[targets.length - 1][1]);
   if (!target || target.dataset.pubkey !== event.pubkey || target.dataset.notice !== undefined) return;
   const updatedAt = Number(event.created_at);
   if (!Number.isFinite(updatedAt) || updatedAt < Number(target.dataset.updatedAt || 0)) return;
   const body = target.querySelector(":scope > div");
   if (!body) return;
   target.dataset.updatedAt = String(updatedAt);
   const attachments = roomAttachments(event);
   body.replaceWith(chatMarkdown(el("div"), attachmentContent(event.content || "", attachments)));
   target.querySelector(":scope > [data-room-attachments]")?.remove();
   const media = attachmentNodes(attachments); if (media) target.insertBefore(media, target.querySelector(":scope > footer"));
   if (target.dataset.edited !== undefined) return;
   target.dataset.edited = "";
   let footer = target.querySelector(":scope > footer");
   if (!footer) { footer = el("footer"); target.append(footer); }
   const mark = el("span", "edited");
   mark.dataset.edited = "";
   footer.append(mark);
 };
 // signAndPublish signs one room event, checks it came back unchanged and
 // valid, and publishes it once.
  const signAndPublish = async (unsigned, signal) => {
   if (typeof tiny.signing?.publish !== "function") throw Error("The event signing API is still loading. Try again.");
   return tiny.signing.publish(unsigned, {signal});
  };

 // RoomCompose sends a chat message (kind 9) or a thread reply (kind 12).
 // Enter sends and Shift+Enter starts a new line.
 class RoomCompose extends FormElement {
   connectedCallback() {
     super.connectedCallback();
     this.renderFiles();
     this.resizeContent();
     if (this.keys) return;
     this.keys = true;
     this.querySelector("[data-attach]")?.addEventListener("click", () => {
       this.form.elements.attachments.click();
     });
     this.addEventListener("input", event => {
       if (event.target.name === "content") this.resizeContent();
     });
     this.addEventListener("keydown", event => {
       if (event.key !== "Enter" || event.shiftKey || event.isComposing || !event.target.matches?.("textarea")) return;
       event.preventDefault();
       this.form?.requestSubmit();
     });
     this.addEventListener("change", event => {
       if (event.target.name !== "attachments") return;
       this.chooseFiles(event.target.files);
       event.target.value = "";
     });
     this.addEventListener("paste", event => {
       if (!this.querySelector("room-files")) return;
       const files = [...(event.clipboardData?.files || [])];
       if (!files.length) return;
       event.preventDefault();
       this.chooseFiles(files);
     });
     this.addEventListener("dragover", event => {
       if (!this.querySelector("room-files") || ![...(event.dataTransfer?.types || [])].includes("Files")) return;
       event.preventDefault();
       if (!this.submitting) this.dataset.over = "";
     });
     this.addEventListener("dragleave", () => { delete this.dataset.over; });
     this.addEventListener("drop", event => {
       delete this.dataset.over;
       if (!this.querySelector("room-files") || !event.dataTransfer?.files?.length) return;
       event.preventDefault();
       this.chooseFiles(event.dataTransfer.files);
     });
   }

   disconnectedCallback() {
     this.uploadController?.abort();
     this.releasePreviews();
   }

   resizeContent() {
     const content = this.form?.elements?.content;
     if (!content) return;
     content.style.height = "auto";
     content.style.height = content.scrollHeight + "px";
   }

   releasePreviews() {
     for (const entry of this.pendingFiles || []) {
       if (entry.preview) URL.revokeObjectURL(entry.preview);
       delete entry.preview;
     }
   }

   busy(on) {
     super.busy(on);
     this.form?.querySelectorAll("input, textarea").forEach(input => { input.disabled = on; });
   }

   chooseFiles(files) {
     try { this.addFiles(files); this.report(""); }
     catch (error) { this.report(error.message, true); }
   }

   addFiles(files) {
     if (this.submitting) return;
     const chosen = [...files];
     const pending = this.pendingFiles || [];
     if (pending.length + chosen.length > 8) throw Error("Attach up to 8 files per message.");
     if (chosen.some(file => file.size === 0 || file.size > 32 * 1024 * 1024)) throw Error("Each attachment must be between 1 byte and 32 MiB.");
     this.pendingFiles = [...pending, ...chosen.map(file => ({file}))];
     this.renderFiles();
   }

   renderFiles() {
     const pending = this.pendingFiles || [];
     const content = this.form?.elements?.content;
     if (content) content.required = pending.length === 0;
     const target = this.querySelector("room-files");
     if (!target) return;
     const list = el("ul");
     pending.forEach((entry, index) => {
       const row = el("li"), remove = el("button", "×");
       remove.type = "button";
       remove.disabled = Boolean(this.submitting);
       remove.setAttribute("aria-label", "Remove " + entry.file.name);
       remove.title = "Remove " + entry.file.name;
       remove.addEventListener("click", () => {
         if (this.submitting) return;
         if (entry.preview) URL.revokeObjectURL(entry.preview);
         this.pendingFiles.splice(index, 1);
         this.renderFiles();
         const next = target.querySelectorAll("button")[Math.min(index, this.pendingFiles.length - 1)];
         (next || this.querySelector("[data-attach]"))?.focus();
       });
       let preview;
       if (entry.file.type.startsWith("image/") && entry.file.type !== "image/svg+xml") {
         entry.preview ||= URL.createObjectURL(entry.file);
         preview = el("img");
         preview.src = entry.preview;
         preview.alt = "";
       } else {
         preview = el("span", entry.file.type.startsWith("video/") ? "Video" : entry.file.type.startsWith("audio/") ? "Audio" : "File");
         preview.dataset.preview = "";
         preview.setAttribute("aria-hidden", "true");
       }
       const name = el("span", entry.file.name);
       name.title = entry.file.name;
       const size = entry.file.size < 1024 * 1024 ? Math.ceil(entry.file.size / 1024) + " KB" : (entry.file.size / (1024 * 1024)).toFixed(1) + " MB";
       row.append(preview, name, el("small", entry.descriptor ? "Uploaded" : size), remove);
       list.append(row);
     });
     target.replaceChildren(...(pending.length ? [list] : []));
   }

   event(content, attachments = []) {
     const room = this.getAttribute("room") || "";
     if (!/^[a-z0-9_-]{1,64}$/.test(room)) throw Error("The room id is missing.");
     const kind = Number(this.getAttribute("kind") || 9);
     if (kind !== 9 && kind !== 12) throw Error("Unsupported message kind.");
     const tags = [["h", room]];
     if (kind === 12 || this.getAttribute("root")) {
       const root = this.getAttribute("root"), author = this.getAttribute("root-pubkey");
       if (!isHex64(root)) throw Error("The thread root is missing.");
       tags.push(kind === 12 ? ["e", root] : ["e", root, "", "root"]);
       if (isHex64(author) && author !== this.getAttribute("pubkey")) tags.push(["p", author]);
     }
     const quote = this.getAttribute("quote");
     if (quote) {
       if (!isHex64(quote)) throw Error("The quoted event is invalid.");
       const relay = this.getAttribute("quote-relay");
       const author = this.getAttribute("quote-pubkey");
       // NIP-10 keeps relay and author positional. Preserve the empty relay
       // slot when only the quoted author's key is known.
       const tag = ["q", quote, relay || ""];
       if (isHex64(author)) {
         tag.push(author);
         if (author !== this.getAttribute("pubkey") && !tags.some(item => item[0] === "p" && item[1] === author)) tags.push(["p", author]);
       }
       tags.push(tag);
     }
     roomMentions(content).forEach(key => { if (!tags.some(tag => tag[0] === "p" && tag[1] === key)) tags.push(["p", key]); });
     const lines = [];
     attachments.forEach(file => {
       tags.push(["imeta", "url " + file.url, "m " + file.type, "x " + file.sha256, "size " + file.size, "filename " + file.filename]);
       const label = file.filename.replace(/[\\[\]]/g, "\\$&");
       lines.push(file.type.startsWith("image/") ? `![image](${file.url})` : file.type.startsWith("video/") ? `![video](${file.url})` : `[${label}](${file.url})`);
     });
     content = [content, lines.join("\n")].filter(Boolean).join("\n\n");
     return {kind, created_at: now(), tags, content};
   }

   async uploadFiles(signal) {
     const pending = this.pendingFiles || [];
     for (const [index, entry] of pending.entries()) {
       if (signal.aborted) throw Error("Upload canceled.");
       if (entry.descriptor) continue;
       const file = entry.file;
       this.report("Uploading " + file.name + " (" + (index + 1) + " of " + pending.length + ")…");
       const bytes = new Uint8Array(await file.arrayBuffer());
       const hash = await tiny.sha256hex(bytes);
       const filename = Array.from(file.name.replace(/[\u0000-\u001f\u007f]/g, " ").trim()).slice(0, 255).join("") || "file";
       const path = "/rooms/" + encodeURIComponent(this.getAttribute("room")) + "/attachments?filename=" + encodeURIComponent(filename);
       const response = await tiny.signedFetch(path, "PUT", bytes, {contentType: file.type || "application/octet-stream", signal});
       if (signal.aborted) throw Error("Upload canceled.");
       const descriptor = await response.json();
       if (descriptor.sha256 !== hash || descriptor.size !== bytes.byteLength) throw Error("The upload descriptor has an unexpected hash or size.");
       let url;
       try { url = new URL(descriptor.url); } catch { throw Error("The upload descriptor has an invalid URL."); }
       const prefix = tiny.localPath("/media/" + hash);
       if (url.origin !== new URL(location.href).origin || url.username || url.password || url.search || url.hash || !(url.pathname === prefix || new RegExp("^" + prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "\\.[a-z0-9]{1,16}$").test(url.pathname))) throw Error("The upload descriptor has an unexpected URL.");
       if (!/^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(descriptor.type || "")) throw Error("The upload descriptor has an invalid media type.");
       entry.descriptor = {...descriptor, filename};
       this.renderFiles();
     }
     return pending.map(entry => entry.descriptor);
   }

   async submit(form) {
     const content = (form.elements.content?.value || "").trim();
     if (!content && !this.pendingFiles?.length) return;
     this.event(content);
     if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
     const controller = new AbortController();
     this.uploadController = controller;
     try {
       const files = await this.uploadFiles(controller.signal);
       if (controller.signal.aborted) throw Error("Upload canceled.");
       this.report("Signing…");
       const event = await signAndPublish(this.event(content, files), controller.signal);
       form.reset();
       this.releasePreviews();
       this.pendingFiles = [];
       this.renderFiles();
       this.resizeContent();
       this.report("");
       roomAppend(event, {room: this.getAttribute("room"), inThread: Boolean(this.getAttribute("root")), own: true});
     } finally { this.uploadController = null; }
   }
 }

 // RoomCreate signs a NIP-29 create (kind 9007) with an id derived from the
 // name, then opens the new room.
 class RoomCreate extends FormElement {
   // event builds the kind 9007 room creation. The id comes from the name
   // unless one is given, so a client that wants a fixed id, such as a Buzz
   // gateway that expects a UUID, can ask for it.
   event({name = "", id = "", about = "", access = "open"} = {}) {
     const title = String(name).trim(), given = String(id).trim().toLowerCase();
     if (given && !/^[a-z0-9_-]{1,64}$/.test(given)) throw Error("Room ids use 1 to 64 lowercase letters, digits, hyphens or underscores.");
     id = given || roomID(title);
     if (!id) throw Error("Enter a name with letters or digits.");
     const tags = [["h", id], ["name", title]];
     if (String(about).trim()) tags.push(["about", String(about).trim()]);
     tags.push(["visibility", access === "members" ? "members" : "open"]);
     return {id, event: {kind: 9007, created_at: now(), tags, content: ""}};
   }

   async submit(form) {
     const value = name => form.elements[name]?.value || "";
     const {id, event} = this.event({name: value("name"), id: value("id"), about: value("about"), access: value("access")});
     this.report("Signing…");
     await signAndPublish(event);
     this.report("Created #" + id + ".");
     form.reset();
     await tiny.navigate?.(roomPath(id), true);
   }
 }

 // RoomAction signs one NIP-29 management event for a room: add a member
 // (9000), remove one (9001), edit the room (9002), join (9021) or leave (9022).
 class RoomAction extends FormElement {
   event(values = {}) {
     const room = this.getAttribute("room") || "";
     if (!/^[a-z0-9_-]{1,64}$/.test(room)) throw Error("The room id is missing.");
     const kind = Number(this.getAttribute("kind"));
     const tags = [["h", room]];
     switch (kind) {
       case 9000: case 9001: {
         const key = keyHex(values.pubkey);
         if (!key) throw Error("Enter an npub or a 64-character hex key.");
         const tag = ["p", key];
         if (kind === 9000) tag.push(["owner", "admin"].includes(values.role) ? values.role : "member");
         tags.push(tag);
         break;
       }
       case 9002: {
         const name = String(values.name || "").trim();
         if (!name) throw Error("Enter a name.");
         tags.push(["name", name], ["about", String(values.about || "").trim()], ["picture", String(values.picture || "").trim()], ["visibility", values.access === "members" ? "members" : "open"]);
         break;
       }
       case 9021: case 9022: break;
       default: throw Error("Unsupported room action.");
     }
     return {kind, created_at: now(), tags, content: ""};
   }

   async submit(form) {
     const values = {};
     for (const field of form.elements) if (field.name) values[field.name] = field.value;
     this.report("Signing…");
     await signAndPublish(this.event(values));
     this.report("Done.");
     await tiny.navigate?.(location.href);
   }
 }

 // RoomLive follows a room's stream: new messages join the timeline, and
 // reactions update their targets. It reconnects with backoff.
 class RoomLive extends HTMLElement {
   static get observedAttributes() { return ["room", "root"]; }
   connectedCallback() { this.open(); roomScroll(); }
   disconnectedCallback() { this.close(); }
   attributeChangedCallback() { if (this.isConnected) { this.close(); this.open(); } }

   open() {
     const room = this.getAttribute("room");
     if (!room || this.source || typeof EventSource !== "function") return;
     this.delay = this.delay || 1000;
     const source = new EventSource(roomPath(room, "/stream"), {withCredentials: true});
     this.source = source;
     source.addEventListener("open", () => { this.delay = 1000; this.textContent = ""; });
     source.addEventListener("message", event => {
       let parsed;
       try { parsed = JSON.parse(event.data); } catch { return; }
       this.receive(parsed);
     });
     source.addEventListener("error", () => {
       if (this.source !== source) return;
       this.close();
       this.textContent = "reconnecting";
       this.timer = setTimeout(() => { this.timer = null; this.open(); }, this.delay);
       this.delay = Math.min(this.delay * 2, 30000);
     });
   }

   close() {
     this.source?.close();
     this.source = null;
     clearTimeout(this.timer);
     this.timer = null;
   }

   receive(event) {
     if (!event || typeof event !== "object") return;
     const room = this.getAttribute("room");
     if (tagValue(event, "h") !== room && !(event.kind === 20001 && !tagValue(event, "h"))) return;
     if (typeof CustomEvent === "function") document.dispatchEvent?.(new CustomEvent("tiny:room-event", {detail: {room, event}}));
     if (event.kind === 7) { roomReact(event); return; }
     if (event.kind === 40003) { roomEdit(event); return; }
     if (!roomKinds.includes(event.kind)) return;
     const root = this.getAttribute("root");
     if (root) {
       const reference = replyRoot(event);
       if (reference !== root && !document.getElementById("msg-" + reference)) return;
     }
     roomAppend(event, {room: this.getAttribute("room"), inThread: Boolean(root)});
   }
 }

  tiny.rooms = Object.freeze({keyHex, roomMentions, roomID, replyRoot, messageNode, roomAppend, roomReact, roomEdit, linkify, chatMarkdown, roomAttachments, attachmentContent, attachmentNodes});
  tiny.roomElements = Object.freeze({RoomCompose, RoomCreate, RoomAction, RoomLive});
  customElements.define("room-compose", RoomCompose);
  customElements.define("room-create", RoomCreate);
  customElements.define("room-action", RoomAction);
  customElements.define("room-live", RoomLive);
})();
