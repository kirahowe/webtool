# webtool

webtool gives relay's research roles
the web: a search, a fetch, and a check that the pages a report cites say
what the report quotes. It is an external tool. relay runs it once per call
and speaks to it in relay's envelope protocol.

Whoever wrote the pages an agent reads can also choose the next URL it
fetches. So fetch treats every URL as chosen by an attacker who wants to
reach something on the operator's machine or network, and refuses anything
that is not plainly on the public internet.

## Building

```sh
make build    # bin/webtool
make test     # gofmt, go vet, and the tests with the race detector
make update   # update dependencies, then test
```

## Running

```sh
webtool --config FILE search|fetch|check
```

The call arrives on stdin as one JSON object, `{input, labels, call_id}`:
the model's arguments, the calling session's labels and the call's id.
webtool prints exactly one `{content, is_error, labels}` on stdout and
nothing else. Diagnostics go to stderr.

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
whole fetch may take, redirects and body included (20 seconds by default),
how much of a body is read (5 MB), how many characters of text the
agent is shown (20000), how much the snapshot directory may hold (2 GB;
past that the oldest snapshots are removed), and how much memory the
process that reads a page's text may use (512 MiB). `search` is needed only by the search command.
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

The output is a few lines about the fetch, any warnings, a blank line and
then the page's text:

```
url: http://iana.org/
final_url: https://www.iana.org/
status: 200
content_type: text/html
sha256: c26b9fd1a5b06c50d063b5c14ca08fe0a31af7ef8bb7869d8db2ab2ef577b311
redirects: http://iana.org/ (301) -> https://www.iana.org/
title: Internet Assigned Numbers Authority

# Internet Assigned Numbers Authority

The global coordination of the DNS Root, IP addressing, and other Internet
protocol resources is performed as the Internet Assigned Numbers Authority
(IANA) functions. Learn more. <https://www.iana.org/about>
```

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
status, content type, fetch time, redirect chain and the counts above, the
URLs with invisible characters escaped. A body fetched twice is kept once,
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

Each redirect is checked like the first URL, at most five are followed,
and the chain is reported.

Some things this cannot see. Stylesheets and classes are not evaluated,
so text hidden by a rule in a `<style>` element, or pushed off screen,
reaches the agent, and check can find a quote in it. A stylesheet can also
add text (`::before` and `::after` content) or reorder it, which a person
reads and webtool does not, so what a person reads can differ from the
text check looks for quotes in. Text can also be unreadable without being
hidden in markup, in a colour that matches its background, say. A public address that leads back to the operator's
network, such as the router's own WAN address reached by hairpin NAT, is
not known to webtool and is not refused.

## check

check is for relay's report verifier. relay calls it itself; it is not a
tool to grant to roles.

check exists to catch a research agent that invents or misquotes a
citation. Against a page author who hides text from people it is best
effort: without a rendering engine it cannot be complete, and
stylesheets, scripts (which can rewrite the page after it loads; check
reads the markup as served) and overlays are known limits. relay never
trusts free text because check found it: only typed fields (dates,
amounts, choices) can be cleared at the trust ladder's first step, and
those are documented as open to outside influence. So check fails
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
  "url": "http://iana.org/help/example-domains",
  "fetched": true,
  "complete": true,
  "uncheckable": "",
  "error": "",
  "status": 200,
  "final_url": "https://www.iana.org/help/example-domains",
  "redirects": [{"url": "http://iana.org/help/example-domains", "status": 301}],
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

`crosses_domains` is true when any step from the first URL through the
redirects to the final URL, or to the redirect target a failed fetch was
sent to, moves to another registrable domain (the eTLD+1
by the public suffix list; an IP address is its own domain). `domain` and
`final_domain` are those of the first and final URLs, in ASCII. For each
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
is not matched. Link targets, image descriptions and the marks fetch adds
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

Add the two tools under `:tools` and grant them to the research roles.
Both are `:egress`: a URL or a query can carry data out, so once a session
holds private data and contaminated text, relay holds each call for the
operator.

```clojure
"web_search" {:class :egress
              :labels ["contaminated"]
              :protocol :envelope
              :timeout-seconds 60
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
             :description "Fetch a web page as text. Links show as text <url>. Local and private addresses are refused."
             :parameters {:type "object"
                          :properties {:url {:type "string" :description "An http or https URL."}}
                          :required ["url"]
                          :additionalProperties false}
             :command ["/path/to/webtool/bin/webtool" "--config" "/path/to/webtool.json" "fetch"]}
```

check runs as `webtool --config /path/to/webtool.json check` with the same
envelope on stdin.

## License

MIT. See [LICENSE](LICENSE).
