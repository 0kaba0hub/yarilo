# Stopword list sources

Each list is its published source **plus** the words yarilo had already added to
it. Recorded here so the next person can check a list against its origin instead
of re-deriving it by intuition — which is what produced the state this file
exists to prevent.

| Language | Source | Words |
|:---|:---|---:|
| en | https://snowballstem.org/algorithms/english/stop.txt | 174 |
| de | https://snowballstem.org/algorithms/german/stop.txt | 235 |
| es | https://snowballstem.org/algorithms/spanish/stop.txt | 309 |
| fr | https://snowballstem.org/algorithms/french/stop.txt | 163 |
| it | https://snowballstem.org/algorithms/italian/stop.txt | 288 |
| pt | https://snowballstem.org/algorithms/portuguese/stop.txt | 206 |
| ru | https://snowballstem.org/algorithms/russian/stop.txt | 162 |
| uk | https://github.com/explosion/spaCy `spacy/lang/uk/stop_words.py` (MIT) + https://github.com/stopwords-iso/stopwords-uk (MIT) | 482 |

Snowball's lists are BSD-licensed, as is the Snowball project itself. The
two Ukrainian sources are MIT. All are compatible with AGPL-3.0.

## What was wrong, and what was not

The lists shipped before were hand-edited copies of these files. English matched
its source word for word; every other language had lost some of it, and Italian
had lost half:

| | was | source | source words missing | words we had beyond it |
|:---|---:|---:|---:|---:|
| en | 174 | 174 | 0 | 0 |
| pt | 204 | 203 | 2 | 3 |
| ru | 151 | 159 | 8 | 0 |
| de | 220 | 231 | 15 | 4 |
| fr | 146 | 154 | 17 | 9 |
| es | 256 | 308 | 52 | 0 |
| it | 146 | 279 | 142 | 9 |

The signature was visible without knowing any of the languages: one form of a
pair present and the other absent, and repeated entries — `que` twice in
Spanish, four repeats in Italian. A list taken whole does not repeat itself.

**But the words beyond the source were not noise.** French Snowball has no
`est` and no `été` — "is" and "been", two of the commonest words in the
language — and yarilo's list had both, with tests relying on them. Replacing
the lists wholesale would have deleted somebody's earlier correction.

So the lists here are the union: nothing that was in either is gone.

## Additions beyond both

Four words complete paradigms whose other members the source already carries, so
the symmetry check does not have to be weakened to accommodate a gap:

| Language | Added | Alongside |
|:---|:---|:---|
| es | `unas` | `un`, `una`, `unos` |
| ru | `оно` | `он`, `она`, `они` |
| ru | `эта` | `этот`, `эти`, `этой`, `этом`, `этого` |
| ru | `мою` | `мой`, `моя` |

## Editing a list

Add or remove a word only with the reason recorded here. Otherwise take the
source file as it is: a list edited without a note is indistinguishable from one
that was damaged, which is the position this started from.

## Ukrainian

`uk` has no Snowball list, because Snowball has no Ukrainian stemmer.

The 73 words shipped before were `stopwords-iso/stopwords-uk`, whole and
unmodified — the one list here that had *not* been hand-edited. Its problem was
the source: it does not contain `і`, `в`, `не` or `на`, four of the most
frequent words in the language.

That matters more for Ukrainian than for any other language here. With no
stemmer, the chain is `lowercase → stopwords → passthrough`, so stopwords are
the only content filter Ukrainian gets, and it was the one that was thinnest.

spaCy's list carries all four and 467 words in total, and covers 58 of the 73.
The 15 it does not carry — `авжеж`, `вздовж`, `всередині`, `дещо`, `замість`,
`отже` among them — are kept, as everywhere else here.
