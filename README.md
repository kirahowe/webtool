# webtool

webtool gives relay's roles
the web: a search, a fetch, an ask that searches and reads the top
results, and a check that the pages a report cites say what the report
quotes. It is an external tool. relay runs it once per call
and speaks to it in relay's envelope protocol.

Whoever wrote the pages an agent reads can also choose the next URL it
fetches. So fetch treats every URL as chosen by an attacker who wants to
reach something on the operator's machine or network, and refuses anything
that is not plainly on the public internet.

## Building

```sh
make build    # bin/webtool
make fixture  # bin/webtool-fixture, for tests only
make test     # gofmt, go vet, and both builds' tests with the race detector
make update   # update dependencies, then test
```

### The fixture build

`make fixture` builds webtool with the `fixture` build tag, for relay's
red-team corpus, which serves attack pages from a local server. This build
may fetch from one loopback origin, named in the config as
`"fetch": {"fixture_origin": "127.0.0.1:8123"}`. Only that address and
port are let through. Every other rule still applies, to the first URL
and to the target of a redirect, so a redirect from a fixture page into
the tailnet or a private range is refused as it would be in the release
build. Nothing follows a redirect, so a target is only judged, never
requested. A
host name never reaches the origin, even one that resolves to it.

Its search reads results from a file instead of calling Brave:
`"search": {"backend": "fixture", "results_file": "/abs/results.json"}`.
The file is a JSON array of objects with `title`, `url` and `snippet`,
and every query gets the first `count` of them.

The release build has none of this. It refuses a config that sets
`fixture_origin`, `results_file` or the fixture backend, and its fetcher
cannot be given an origin. Never give relay's real tools the fixture
binary.

## Running

```sh
webtool --config FILE search|fetch|check|ask
```

The call arrives on stdin as one JSON object, `{input, labels, call_id}`:
the model's arguments, the calling session's labels and the call's id.
Of the call's labels webtool keeps `private` and `contaminated` and ignores
any other, such as a label naming where something came from. webtool prints
exactly one `{content, is_error, labels}` on stdout and nothing else.
Diagnostics go to stderr.

Everything webtool returns from the web carries the label `contaminated`,
refusals included. A call it cannot read (bad JSON, a repeated key, an
unknown argument) gets `is_error` and no labels, since nothing was fetched.
A problem with the settings, such as a missing config file or no search
backend, exits with status 1 and prints nothing on stdout. relay then tells
the model only that the tool failed, and the reason goes to relay's log.

webtool reads no environment variables. Proxy settings such as
`HTTP_PROXY` are ignored.

## The config file

```json
{
  "snapshot_dir": "/Users/kira/Library/Application Support/webtool/snapshots",
  "fetch": {
    "timeout_seconds": 20,
    "max_body_bytes": 5000000,
    "max_text_chars": 20000,
    "max_snapshot_bytes": 2000000000,
    "max_read_memory_bytes": 536870912,
    "extract_sandbox": "required"
  },
  "search": {
    "backend": "brave",
    "api_key_command": ["security", "find-generic-password", "-s", "relay-brave", "-w"],
    "timeout_seconds": 20
  }
}
```

`webtool.example.json` holds the same. An unknown key is an error, so a
misspelt limit is never quietly the default.

`snapshot_dir` is required and must be absolute. fetch and check keep
every page they read there. The `fetch` limits apply to both: the time a
whole fetch may take, body included (20 seconds by default),
how much of a body is read (5 MB), how many characters of text the
agent is shown (20000), how much the snapshot directory may hold (2 GB;
past that the oldest snapshots are removed), and how much memory the
process that reads a page's text may use (512 MiB). `search` is needed only by the search and ask commands.
`api_key_command` is run without a shell, with only `PATH` and `HOME` in
its environment, and its output, trimmed, is the API key. Keep the key in
the keychain:

```sh
security add-generic-password -s relay-brave -a relay -w
```

## search

Input: `{"query": "...", "count": 10}`. `query` is 1 to 400 characters.
`count` is optional, 1 to 20, 10 by default.

The one backend is the [Brave Search API](https://brave.com/search/api/).
The output is numbered results:

```
1. netip package - net/netip
   https://pkg.go.dev/net/netip
   Package netip defines an IP address type that's a small value type.

2. ...
```

The request ignores proxies and follows no redirect, since a redirected
request would carry the key to wherever it pointed.

## fetch

Input: `{"url": "https://..."}`.

fetch follows no redirect. The output for a page is a few lines about the
fetch, any warnings, a blank line and then the page's text:

```
url: https://www.iana.org/
final_url: https://www.iana.org/
status: 200
content_type: text/html
sha256: c26b9fd1a5b06c50d063b5c14ca08fe0a31af7ef8bb7869d8db2ab2ef577b311
title: Internet Assigned Numbers Authority

# Internet Assigned Numbers Authority

The global coordination of the DNS Root, IP addressing, and other Internet
protocol resources is performed as the Internet Assigned Numbers Authority
(IANA) functions. Learn more. <https://www.iana.org/about>
```

A response with a 3xx status and a `Location` header is a short, successful
result that says the page moved. The target is resolved against the URL
that was fetched, so it is always absolute, and it is written exactly as
it should be fetched, alone on its own line:

```
url: http://iana.org/
status: 301

This page has moved. It was not followed. Its new address is:

https://www.iana.org/
```

The target goes through every rule below, as any URL does, but it is never
requested, not even resolved. If a rule refuses it, the result is an error
naming the target and the rule. A target with an invisible character, or with any character
Unicode counts as a space (a no-break or ideographic space as much as an
ASCII one), that the URL writer would leave raw is refused too, so the
line the agent sees is the address, byte for byte. No body is read and no snapshot is
kept for a moved page. Fetching the address is a new call, and relay can
check it against the URLs it has shown the agent. A 3xx with no `Location`
is an ordinary page.

A page that is not a 2xx still comes back, with its status, so the agent
can read a 404 page. Text past `max_text_chars` is cut, with a note saying
how much there was, and so is a title longer than that or than 1000
characters. Reading the text stops once it is as long as `max_body_bytes`,
so link targets cannot make a small page into a large text. A page that
nests elements more than 512 deep is not read at all, and one with more
than 200,000 `<` characters (at most 400,000 tags, text runs and comments)
is read only that far, since the HTML parser's cost grows with the square
of adjacent text runs. Counting `<` rather than tokens means no reading of
raw text can let a page past the limit. Each adds a warning. `max_body_bytes` may be at most 256 MiB.

The parser has other inputs whose cost grows far faster than their size,
and it cannot be stopped once started, so a page's text is read in a
child process: webtool runs itself with a hidden subcommand, sends the
body on stdin, its exact length in a header the child checks, and reads
the text back as strict JSON, of a bounded size.
The child gets no environment and, on macOS, runs under `sandbox-exec`
with no network and no file writes. `fetch.extract_sandbox` is
`"required"` by default on macOS: if `sandbox-exec` is missing or cannot
apply the profile, fetch and check exit 1 with the reason on stderr
rather than read a page unsandboxed. `"off"` (the default elsewhere, since
only macOS has the tool) is for testing inside another sandbox. The child
watches its own memory and exits past `max_read_memory_bytes`, which the
call reports as "the page was too costly to read"; webtool kills it at
the fetch's time limit, which reads "timed out reading the page's text";
any other failure reads "could not read the page", with the reason and
the end of the child's stderr on webtool's stderr. What comes back is
bounded too: the agent's text cut at `max_text_chars`, and the visible
text check needs, without the characters quote matching ignores, in all
at most about three times the body. One page's text is read at a time. A body cut at `max_body_bytes` is
read only up to its last `>`, so a cut entity or tag cannot make a word the
page does not have. A refusal or a failure comes back as `is_error` with a
plain reason, such as `refused: 10.0.0.1 is a private address`.

HTML becomes text with paragraphs, headings, list items and table cells
kept apart. Links read `text <absolute-url>` so the agent can follow them,
and an image reads `[image: alt text]`. Scripts, styles and templates are
dropped. So are elements a person would not see: a `<dialog>` that is not
open, all but the summary of a `<details>` that is not open (in a named
group only the first open member is), a `popover`, an `<rp>`, an element
with a namespace prefix such as XHTML's `h:style`, and any
element hidden by the `hidden` attribute (unless its own style sets
`display`), `aria-hidden="true"`, or an inline style.

The inline style is tokenised as CSS is: comments separate tokens, strings
and brackets keep their semicolons, and an escape is never syntax. `var()`
is resolved against the custom properties set inline on the element and
its ancestors, each computed where it is set, with a budget on how far a
value may grow. The last declaration of a property wins unless an earlier
one is `!important`, and the `all`, `font` and `overflow` shorthands take
part. The element is hidden by `display:none`, `visibility:hidden` or
`collapse`, `content-visibility:hidden`, an opacity below 0.1, a font size
below 2px or 0.1em, a colour or `-webkit-text-fill-color` with alpha below
0.1, a zero scale on either axis, an absolutely positioned clip to nothing,
any clip-path or mask, `-webkit-text-security`, a zero height or width on
an axis that clips, a text indent of -100px or less, a margin or offset at
least 999px off the page, `display:table-column`, or `content` that
replaces it with an image. Opacity and font size compound down the tree,
so three nested opacities of 0.1 hide, and so does a font two levels of
relative sizes make smaller than 2px. Visibility and
colour are inherited, so a child can make its text visible again.

These rules lean toward hiding: when unsure, the agent loses a little
visible text rather than see text a person would not. A later declaration
that would show the element counts only if webtool reads it as valid, so
a value a browser drops cannot undo an earlier one that hid. A value
webtool cannot read for a property that can hide (an unresolved `var()`, a
`calc()` opacity, a clip-path polygon) hides. So does a style over 16 KB or
one nesting brackets more than 32 deep. A colour set by an unresolved
`var()` is the exception: it is left alone, since hiding all text coloured
that way would hide whole sites.
The characters dropped from hidden elements are counted in a warning.
Characters that do not print, or change how the text around them prints,
are escaped as `\u{XXXX}` and counted in a warning: zero-width characters,
bidi controls, Unicode tag characters, private use and unassigned code
points, variation selectors and other default-ignorable code points, and
any other control or format character except newline and tab.

Only `text/html`, `application/xhtml+xml` and `text/plain` are read. A body
with no content type is sniffed. PDFs are refused: the pure-Go PDF readers
are small projects, and a parser for a format that large is a poor thing to
put in front of hostile input.

Each page read is kept in `snapshot_dir`, named by the SHA-256 of its raw
body: the body as `<sha256>.body`, the text the agent was shown (up to
`max_text_chars`) as `<sha256>.txt`, and `<sha256>.json` with the URL, final URL,
status, content type, fetch time and the counts above, the URLs with
invisible characters escaped. A body fetched twice is kept once,
and its JSON describes the latest fetch. When the directory holds more than
`max_snapshot_bytes`, the oldest snapshots are removed, never the one just
written. Only files named as snapshots are counted or removed, and a lock
on `.lock` in the directory keeps runs that share it from removing each
other's files half way. A fetch waits for the lock no longer than its time
limit.

### What fetch refuses, and why

The agent calling fetch has read text written by strangers, so the URL may
be an attacker's. The targets worth protecting are on the operator's side:
a local server that acts on a GET (an ntfy server on 127.0.0.1:2586 takes
a publish as a GET), the tailnet, the router, a cloud metadata address.

fetch speaks only http and https, to ports 80 and 443, with GET. It sends a
fixed User-Agent and no cookies, credentials, Referer or proxy. A URL with
a user name or password is refused.

Before anything is resolved, the host name is refused if it is local by
its form: `localhost` and `*.localhost`, `*.local`, `*.internal`,
`*.ts.net`, `*.home.arpa` and the rest of `.arpa`, `*.lan`, `*.home`,
`*.localdomain`, `*.intranet`, `*.corp`, `*.private`, the reserved `*.test`,
`*.example`, `*.invalid` and `*.onion`, and any name without a dot, which a
resolver may complete with a local search domain. The name is judged as the
HTTP client will dial it, after IDNA mapping and without a trailing dot, so
`ＬＯＣＡＬＨＯＳＴ` and `localhost.` are refused too. A host whose last
label is a number is refused, since `getaddrinfo` reads `2130706433`,
`0x7f.1`, `0177.0.0.1` and `127.1` as 127.0.0.1.

Then the address is checked where it is dialed. A host is refused if any
address it resolves to is refused, and the dialer's `Control` hook checks
the socket's own address once more as it connects, so DNS rebinding cannot
slip a private address past an earlier check. Refused addresses are
loopback, unspecified, the private ranges, link-local (169.254/16 and
fe80::/10), 100.64.0.0/10 (CGNAT, and Tailscale's range), unique-local
fc00::/7 (Tailscale's IPv6 range is in it), multicast, 0.0.0.0/8,
192.0.0.0/24, the documentation and benchmarking ranges, 240.0.0.0/4 and
broadcast, Azure's host service address 168.63.129.16, deprecated and special-purpose IPv6 space, and anything outside
IPv6 global unicast. An IPv6 address that carries an IPv4 one
(IPv4-mapped, IPv4-compatible, NAT64 64:ff9b::/96, 6to4) is judged by the
address it carries. Every address on this machine's interfaces is refused
with its whole prefix, public or not, since a server listening on all
interfaces answers on each and an on-link prefix holds the other devices
on the LAN. A global IPv6 address is refused with its whole /56, the
block a home router is usually delegated and splits among the networks
in the house. Prefixes delegated beyond that /56 are not known here.

A redirect target is checked like the first URL, and then reported, not
followed.

Some things this cannot see. Stylesheets and classes are not evaluated,
so text hidden by a rule in a `<style>` element, or pushed off screen,
reaches the agent, and check can find a quote in it. A stylesheet can also
add text (`::before` and `::after` content) or reorder it, which a person
reads and webtool does not, so what a person reads can differ from the
text check looks for quotes in. Text can also be unreadable without being
hidden in markup, in a colour that matches its background, say. A public address that leads back to the operator's
network, such as the router's own WAN address reached by hairpin NAT, is
not known to webtool and is not refused.

## ask

Input: `{"query": "...", "n": 3}`. `query` is as for search. `n` is
optional, 1 to 5, 3 by default.

ask searches, then fetches the first `n` result URLs, each once, through
the same guarded fetcher as fetch. There is no URL argument: the only
URLs fetched are the ones the search backend returned, so the one thing
the caller chooses is the query. When a result moved, ask follows the
redirect only if the target appears, character for character, among the
URLs the search returned (any of the `2n`, not only the first `n`), and
stops after five hops. Any other target is not fetched: that page comes
back as fetch reports a moved page, with the target on its own line. It asks the backend for twice `n`
results, so a URL that comes twice still leaves `n` pages. Pages are read
one at a time, so allow `:timeout-seconds` for a search and five fetches
(a followed redirect is one more fetch).

The output numbers the pages from 1 in search order. Each page starts
with a line holding a random token made for this call, which page text
cannot guess, then the search result's title and URL, then the page as
fetch renders it, or the reason it could not be read:

```
2 pages for "zeffy agent support". Each page starts with a line "=== page N 3f9c0a1e5b7d2468 ===".

=== page 1 3f9c0a1e5b7d2468 ===
result_title: Zeffy help centre
result_url: https://support.zeffy.com/
url: https://support.zeffy.com/
final_url: https://support.zeffy.com/
status: 200
...

=== page 2 3f9c0a1e5b7d2468 ===
result_title: ...
result_url: http://10.0.0.1/
error: refused: 10.0.0.1 is a private address
```

The output is labelled `contaminated`. The call is an error when no page
could be read, and so is a failed search. No results is not an error.

## check

check is not a tool to grant to roles. It was written for a report
verifier, which relay no longer has; it stays as a command that a program
can call to check quotes against pages.

check exists to catch a research agent that invents or misquotes a
citation. Against a page author who hides text from people it is best
effort: without a rendering engine it cannot be complete, and
stylesheets, scripts (which can rewrite the page after it loads; check
reads the markup as served) and overlays are known limits. So check fails
closed. When the page has a trick its rules do not cover, the text is
unchecked and its quotes are not found, rather than check gaining a rule
for each trick.

Input: `{"items": [{"url": "...", "quotes": ["...", "..."]}]}`, 1 to 20
items, each with up to 20 quotes of up to 2000 characters. Each URL is
fetched in turn through the same guarded fetcher as fetch, so allow
`:timeout-seconds` for twenty fetches.

`content` is JSON. Every key is always present; a fetch that failed leaves
what it never reached at zero values (`final_host` is `null`) and says why
in `error`. Items come back in the order given, and so do quotes. Every
string has invisible characters escaped as `\u{XXXX}`.

```json
{"items": [{
  "url": "https://www.iana.org/help/example-domains",
  "fetched": true,
  "complete": true,
  "uncheckable": "",
  "error": "",
  "status": 200,
  "final_url": "https://www.iana.org/help/example-domains",
  "moved_to": "",
  "crosses_domains": false,
  "domain": "iana.org",
  "final_domain": "iana.org",
  "host": {"name": "iana.org", "punycode": false, "unicode": "iana.org",
           "scripts": ["Latin"], "mixed_scripts": false},
  "final_host": {"name": "www.iana.org", "punycode": false, "unicode": "www.iana.org",
                 "scripts": ["Latin"], "mixed_scripts": false},
  "sha256": "bc819241c144d38d2e234a151bfc487358893f98bb0a5ef265127895c538113a",
  "hidden_chars": 0,
  "quotes": [{"quote": "These domains may be used as illustrative examples",
              "found": true, "hidden_chars": false}]
}]}
```

check follows no redirect. When a page moved, `moved_to` holds the
absolute target, `fetched` is false, `error` says the page moved and
redirects are not followed, and every quote is reported not found. The
target is never requested. `crosses_domains` is true when `moved_to` is
on another registrable domain than the URL (the eTLD+1
by the public suffix list; an IP address is its own domain). `domain` and
`final_domain` are those of the first and the answering URLs, in ASCII. For each
host, `punycode` says whether a label starts with `xn--`, `unicode` is how
the name displays, `scripts` are the scripts of its letters, and
`mixed_scripts` is true when one label mixes scripts beyond the
combinations UTS #39 treats as one writing system (Latin with Han and
Japanese kana, with Han and Bopomofo, or with Han and Hangul). So
`xn--pple-43d.com`, a Cyrillic а before Latin "pple", is mixed.

A quote is `found` when it occurs in the page's title, or in one run of
its visible text, after both are normalised. A quote does not run from the
title into the body.

Some pages cannot be checked at all, and `uncheckable` then says why and
every quote is not found: an XHTML page, a page that gives more than one
content type, a page longer than `max_body_bytes` (the rest could hold
anything), a page that refreshes to another within five seconds (by a
`Refresh` header or a meta refresh; 5.9 seconds counts as 5, as browsers
read it), and a page where any element can leave the flow or paint over
other text (a `position` other than static, a float, a transform, a
motion path, a negative margin, a `z-index`, an opacity below 1, an SVG
or replaced element whose overflow is visible, an open `<dialog>`, or
MathML with `mpadded` or `mathbackground`). One such element could cover
any text on the page.
These are judged on the parsed tree, which alone knows what is raw text
(a `<style>` in HTML) and what is markup (a `<style>` in SVG, where a
`<div>` breaks back out into HTML). A page read only in part, cut at
`max_body_bytes` or at the token limit, is uncheckable, since what lies
past the cut could cover what was read.

Elsewhere check vouches only for text inside a fixed set of plain text
elements (`p`, `div`, `span`, `a`, `em`, `strong`, headings, lists, table
cells and the like). Neither the element nor any ancestor may have a
direction but `ltr`; a `hidden`, `popover`, `align` or legacy presentation
attribute; a list numbering attribute (`start`, `reversed`, `value`,
`type`); a size attribute over 2000; a heading inside a heading; a table
whose columns are styled or sized; or an inline style outside a short
list: an opaque colour or background colour (no system colours), font
weight, style and variant, an absolute font size from 8px to 200px, a line
height of `normal` or a number from 1 to 3, alignment, decoration, text
transform, white space, `block` display on blocks or `inline` on inline
elements (not `list-item`, whose marker is not text), list style, cursor,
and on blocks only, a minimum height, padding, borders and margins up to
2000px. No widths, no `var()`. Text anywhere else is read for the agent,
under the hiding rules above, but never matched, and the visible text
breaks around it. It also breaks between table cells, at each list
marker, around elements that show something other than their text (an
image, a form control, a frame, SVG or MathML), around anything not read
(a script, a style, a template), and in place of hidden text, so no quote
runs across any of them. Bidi controls in the text break it too,
paired as the Unicode bidi algorithm pairs them, and what lies inside them
is not matched. NEL and the Unicode line and paragraph separators break
it as well, since none of them is a space in HTML. NEL and the paragraph
separator also end a paragraph, and so every bidi control open before
them. Link targets, image descriptions and the marks fetch adds
for the agent are not part of the visible text. The cost is that a quote in
richly styled text, or across one of those breaks, is not found.

Normalising drops invisible characters, applies Unicode NFKC, folds case,
maps curly quotes and dashes to ASCII and collapses whitespace. `quote`
echoes the quote with invisible characters escaped, and `hidden_chars`
says whether it held any. `complete` is true when the whole page was read:
the body was not cut at `max_body_bytes`, neither text reached its limit,
the title was not cut, and nothing was nested too deeply. A quote not found
on an incomplete page may be in the part not read. `hidden_chars` on the
item is the count of hidden text dropped from the page.

## In relay.edn

Add the tools under `:tools` and grant them to a scout, a role with
`:public true`. They are `:egress`: the query or the URL goes out to
someone outside, so relay lets only a public role have one. A public role
is shown nothing private, so there is nothing of the operator's for it to
send. No call is held for anyone, and there is no quarantine and no
reader.

relay checks every `:egress` call a scout makes before it runs. A `url`
argument must appear character for character in an earlier result of an
`:egress` tool in the same session. So must any `://` address in a query,
from its scheme to the end of the query: webtool echoes a query back, so
nothing may follow the address. A scout can fetch what a search or a page showed it, and a
moved page's new address, but not an address it made up, which could carry
something the steward told it. A redirect is never followed, so its
target is fetched only by a new call, which relay checks the same way.

relay can make that check only on a call it reads, so an `:egress` tool
is an `:envelope` tool with its `:parameters` listed. Each parameter is
`url`, a number or a yes-or-no value, or free text named in `:queries`.
List a parameter there only when its text goes to a service you trust as
you trust the model provider, as search's query goes to the search
provider.

```clojure
"web_search" {:class :egress
              :labels ["contaminated"]
              :protocol :envelope
              :timeout-seconds 60
              :queries ["query"]
              :description "Search the web. Returns numbered results with title, URL and snippet."
              :parameters {:type "object"
                           :properties {:query {:type "string" :description "What to search for, up to 400 characters."}
                                        :count {:type "integer" :description "How many results, 1 to 20; 10 by default."}}
                           :required ["query"]
                           :additionalProperties false}
              :command ["/path/to/webtool/bin/webtool" "--config" "/path/to/webtool.json" "search"]}
"web_fetch" {:class :egress
             :labels ["contaminated"]
             :protocol :envelope
             :timeout-seconds 60
             :description "Fetch a web page as text. Give a URL exactly as a search result or an earlier page showed it. Links show as text <url>."
             :parameters {:type "object"
                          :properties {:url {:type "string" :description "An http or https URL from an earlier result."}}
                          :required ["url"]
                          :additionalProperties false}
             :command ["/path/to/webtool/bin/webtool" "--config" "/path/to/webtool.json" "fetch"]}
```

```clojure
:grants {"scout" {:public true :tools ["web_search" "web_fetch"]}}
```

ask can be granted to a scout the same way, as an `:egress` tool with a
`query`, listed in `:queries`, an optional integer `n`, and a
`:timeout-seconds` of about 150. It
takes no URL, so the only thing the scout chooses is the query.

check runs as `webtool --config /path/to/webtool.json check` with the same
envelope on stdin. It is not a tool to grant to a role.

## License

MIT. See [LICENSE](LICENSE).
