// Package build exposes build-time metadata stamped via -ldflags.
package build

// Version is stamped at build time via -ldflags="-X github.com/yarilomail/yarilo/pkg/build.Version=<tag>".
// Falls back to "dev" for local / untagged builds.
var Version = "dev"

// ChartVersion is the version of helm/Chart.yaml the image was built from,
// stamped the same way.
//
// It exists to catch a deploy where the image and the chart do not come from
// one commit -- `helm upgrade --set image.tag=X` from a working copy on another
// branch, which is how a gate run got a binary expecting a config key that the
// chart in the checkout did not render. The symptom read as the code taking the
// wrong setting; the cause was a template that was not there (#1509).
//
// Compared against the chart_version the ConfigMap carries. Both are the
// chart's own version rather than the image tag, so they are equal on develop,
// where the chart stands still while dev images are numbered, and on master,
// where they rise together.
//
// It therefore catches a chart from another RELEASE, not every difference of
// commit: two images built a week apart on develop carry the same value. The
// case worth catching is the older chart, whose templates do not render keys a
// newer binary reads.
var ChartVersion = "dev"
