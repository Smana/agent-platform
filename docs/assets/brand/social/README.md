# Social preview

`github-social-preview.png` is the GitHub social preview, 1280×640 PNG.

The input supplied in the original brand kit zip had a placeholder repository URL in its
bottom-right corner; that input is not published. This file is the supplied raster with only the
URL region replaced: an Ink `#0A0A0F` patch covers the old text, and `github.com/Smana/maku` is set
in Noto Sans 24 px, Muted `#9B9AAE`. The lockup, tagline and clearspace are untouched.

Regenerate from the supplied input (kept in `/tmp/opencode/maku-brand-inspect/`), from the
repository root:

```bash
magick /tmp/opencode/maku-brand-inspect/social/github-social-preview.png \
  -fill '#0A0A0F' -draw 'rectangle 740,535 1240,610' \
  -font /usr/share/fonts/noto/NotoSans-Regular.ttf -pointsize 24 \
  -fill '#9B9AAE' -gravity southeast \
  -annotate +120+55 'github.com/Smana/maku' \
  docs/assets/brand/social/github-social-preview.png
```

Noto Sans is used because it is the font available locally for a reproducible result; the supplied
lockup text is raster and cannot be re-set.

Publication gate: uploading this image as the repository social preview is a manual step in the
GitHub repository settings, done only once the `github.com/Smana/maku` repository exists (until
then the image advertises a 404) and after a maintainer has viewed it at 100%.
