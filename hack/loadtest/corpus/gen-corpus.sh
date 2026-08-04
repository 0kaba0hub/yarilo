#!/bin/sh
# Build an mbox of non-English mail for the load generator.
#
# Why this exists: the corpus generator in yarilo-loadtest emits English, and
# the English stopword list was the one already correct — 174 words, matching
# its source exactly, unchanged by #1021 and #1025. So a sandbox run measures
# none of the code those changed, by construction (#1046).
#
# The function words come from yarilo's own stopword lists, which is the point:
# what is being measured is how much work disappears when they are filtered
# before stemming rather than after. Taking them from the same files the server
# uses means the corpus and the server cannot disagree about what a stopword is.
#
# The content words are pairs of function words run together — deterministic,
# valid UTF-8, and not themselves in the list. That is deliberate rather than a
# shortcut: the measurement is about how many tokens survive filtering and what
# they cost to stem, not about meaning, and inventing prose would put an
# unstated word-frequency distribution into a number that is supposed to be
# about the filter.
#
# Built from whole words rather than from letters because awk slices bytes: a
# content word cut out of a UTF-8 alphabet by character position is mojibake,
# and a corpus of broken encoding measures the decoder rather than the filter.
#
#   ./gen-corpus.sh uk 200 > /tmp/uk.mbox
#
# The ratio matters and is stated: STOPWORD_RATIO of every ten words is a
# function word, which is roughly what running text carries.
set -eu

lang="${1:?usage: gen-corpus.sh <lang> <messages> [> out.mbox]}"
count="${2:-200}"
ratio="${STOPWORD_RATIO:-4}"   # function words per ten
words_per_line="${WORDS_PER_LINE:-9}"
lines_per_message="${LINES_PER_MESSAGE:-120}"

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
list="$root/internal/fts/language/data/stopwords_$lang.txt"
[ -r "$list" ] || { echo "no stopword list for '$lang' at $list" >&2; exit 1; }

# Lines are wrapped well under the limit. SMTP carries at most 1000 octets per
# line including the CRLF, and a server given a longer one rejects the WHOLE
# transaction — one unwrapped line fails every delivery in the run, which is
# how this cost a day before the loader learned to refuse such a file.
awk -v lang="$lang" -v count="$count" -v ratio="$ratio" \
    -v perline="$words_per_line" -v lines="$lines_per_message" '
BEGIN { srand(20260805) }
{ for (i = 1; i <= NF; i++) stop[++n] = $i }
END {
    if (n == 0) { print "stopword list is empty" > "/dev/stderr"; exit 1 }

    # A content vocabulary of whole words joined in pairs: valid UTF-8 in the
    # same script, long enough to stem, and not in the stopword list itself.
    for (v = 1; v <= 400; v++) {
        a = stop[((v * 17) % n) + 1]
        b = stop[((v * 41 + 7) % n) + 1]
        content[v] = a b
    }

    for (m = 1; m <= count; m++) {
        printf "From corpus@%s.invalid Mon Jan  1 00:00:00 2026\n", lang
        printf "From: gen@%s.test\n", lang
        printf "To: u1@d00001.test\n"
        printf "Subject: corpus %s %d\n", lang, m
        printf "Date: Wed, 05 Aug 2026 09:%02d:00 +0200\n", m % 60
        printf "Content-Type: text/plain; charset=utf-8\n"
        printf "\n"
        for (l = 1; l <= lines; l++) {
            line = ""
            for (w = 1; w <= perline; w++) {
                if ((w + l + m) % 10 < ratio) token = stop[((m * 7 + l * 13 + w * 3) % n) + 1]
                else                          token = content[((m * 11 + l * 5 + w * 23) % 400) + 1]
                line = (line == "") ? token : line " " token
            }
            print line
        }
        printf "\n"
    }
}' "$list"
