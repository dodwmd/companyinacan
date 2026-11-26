// Page checks evaluated through the DevTools protocol (so the page's CSP does not apply to them).
// Called with {width, touch}: the device width in CSS pixels and whether it is a touch screen.
// Returns a list of problem strings.
((opts) => {
  const out = [];
  const add = (s) => { if (out.length < 40 && !out.includes(s)) out.push(s); };
  const desc = (el) => {
    let s = el.tagName.toLowerCase();
    if (el.id) s += '#' + el.id;
    const cls = (el.getAttribute('class') || '').trim();
    if (cls) s += '.' + cls.split(/\s+/).join('.');
    if (el.name && typeof el.name === 'string') s += '[name=' + el.name + ']';
    if (el.tagName === 'A') s += '[href=' + (el.getAttribute('href') || '').slice(0, 40) + ']';
    return s;
  };
  const visible = (el) => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    return el.getClientRects().length > 0;
  };
  const hiddenFromAT = (el) => !!el.closest('[aria-hidden="true"],[hidden]');
  // Content moved out of sight on purpose (skip link, screen-reader-only text).
  const offscreen = (el) => {
    const r = el.getBoundingClientRect();
    return r.bottom <= 0 || r.right <= 0 || (r.width <= 1 && r.height <= 1);
  };

  // --- document ---
  if (!document.documentElement.lang) add('a11y: <html> has no lang');
  if (!document.title.trim()) add('a11y: empty <title>');
  if (!document.querySelector('meta[name="viewport"]')) add('layout: no meta viewport');
  if (!document.querySelector('main,[role="main"]')) add('a11y: no main landmark');
  const h1 = [...document.querySelectorAll('h1')].filter(visible);
  if (h1.length === 0) add('a11y: no h1');
  let last = 0;
  for (const h of document.querySelectorAll('h1,h2,h3,h4,h5,h6')) {
    if (!visible(h)) continue;
    const n = Number(h.tagName[1]);
    if (last && n > last + 1) add('a11y: heading level jumps from h' + last + ' to h' + n + ' (' + h.textContent.trim().slice(0, 40) + ')');
    if (!h.textContent.trim()) add('a11y: empty heading ' + desc(h));
    last = n;
  }

  // --- ids and references ---
  const seen = new Set();
  for (const el of document.querySelectorAll('[id]')) {
    if (seen.has(el.id)) add('a11y: duplicate id ' + el.id);
    seen.add(el.id);
  }
  for (const attr of ['aria-labelledby', 'aria-describedby', 'aria-controls']) {
    for (const el of document.querySelectorAll('[' + attr + ']')) {
      for (const id of el.getAttribute(attr).split(/\s+/).filter(Boolean)) {
        if (!document.getElementById(id)) add('a11y: ' + attr + ' of ' + desc(el) + ' names a missing id ' + id);
      }
    }
  }
  for (const l of document.querySelectorAll('label[for]')) {
    if (!document.getElementById(l.htmlFor)) add('a11y: label for a missing id ' + l.htmlFor);
  }

  // --- accessible names ---
  const textOf = (el) => (el.getAttribute('aria-label') || el.textContent || '').trim() ||
    [...el.querySelectorAll('img[alt]')].map((i) => i.alt).join('').trim() || (el.getAttribute('title') || '').trim();
  for (const el of document.querySelectorAll('input,select,textarea')) {
    if (['hidden', 'submit', 'button', 'reset'].includes(el.type) || hiddenFromAT(el) || !visible(el)) continue;
    const named = (el.labels && el.labels.length && [...el.labels].some((l) => l.textContent.trim())) ||
      el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') || el.getAttribute('title');
    if (!named) add('a11y: form control without a label: ' + desc(el));
  }
  for (const el of document.querySelectorAll('a[href],button,summary')) {
    if (visible(el) && !hiddenFromAT(el) && !textOf(el) && !el.getAttribute('aria-labelledby')) add('a11y: no accessible name: ' + desc(el));
  }
  for (const el of document.querySelectorAll('img')) {
    if (!el.hasAttribute('alt')) add('a11y: img without alt');
  }
  for (const t of document.querySelectorAll('table')) {
    if (visible(t) && !t.querySelector('th') && t.getAttribute('role') !== 'presentation') add('a11y: table without header cells ' + desc(t));
  }

  // --- colour contrast (WCAG AA) ---
  const rgba = (c) => {
    const m = c.match(/rgba?\(([^)]+)\)/);
    if (!m) return null;
    const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number);
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  };
  const lum = (c) => {
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };
  const dark = matchMedia('(prefers-color-scheme: dark)').matches && /dark/.test(getComputedStyle(document.documentElement).colorScheme + (document.querySelector('meta[name="color-scheme"]') || { content: '' }).content);
  const background = (el) => {
    for (let e = el; e; e = e.parentElement) {
      const c = rgba(getComputedStyle(e).backgroundColor);
      if (c && c.a > 0.5) return c;
    }
    return dark ? { r: 18, g: 18, b: 18 } : { r: 255, g: 255, b: 255 };
  };
  for (const el of document.body.querySelectorAll('*')) {
    if (!visible(el) || offscreen(el) || hiddenFromAT(el) || el.disabled) continue;
    const own = [...el.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim());
    if (!own) continue;
    const st = getComputedStyle(el);
    const fg = rgba(st.color);
    if (!fg) continue;
    const bg = background(el);
    const a = lum(fg), b = lum(bg);
    const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
    const size = parseFloat(st.fontSize);
    const large = size >= 24 || (size >= 18.66 && Number(st.fontWeight) >= 700);
    if (ratio < (large ? 3 : 4.5) - 0.05) {
      add('a11y: contrast ' + ratio.toFixed(2) + ' in ' + (dark ? 'dark' : 'light') + ' scheme: ' + desc(el) + ' ' + st.color + ' on rgb(' + bg.r + ', ' + bg.g + ', ' + bg.b + ')');
    }
  }

  // --- phone layout ---
  if (opts.touch) {
    // A mobile browser widens the layout viewport to fit overflowing content, so compare with the
    // device width, not with innerWidth.
    const sw = Math.max(document.documentElement.scrollWidth, window.innerWidth);
    if (sw > opts.width + 1) {
      const culprits = [];
      const clipped = (el) => {
        for (let e = el.parentElement; e && e !== document.body; e = e.parentElement) {
          if (['auto', 'scroll', 'hidden', 'clip'].includes(getComputedStyle(e).overflowX)) return true;
        }
        return false;
      };
      for (const el of document.body.querySelectorAll('*')) {
        const r = el.getBoundingClientRect();
        if (visible(el) && r.right > opts.width + 1 && !clipped(el)) culprits.push(desc(el) + ' (right ' + Math.round(r.right) + ')');
      }
      add('layout: horizontal overflow on a ' + opts.width + 'px phone: page is ' + sw + 'px; ' + culprits.slice(-6).join(', '));
    }
    // WCAG 2.2 target size (minimum): 24 by 24 CSS pixels for controls; links in running text are exempt.
    for (const el of document.querySelectorAll('button,select,input,textarea,summary,header nav a')) {
      if (el.type === 'hidden' || !visible(el) || offscreen(el) || hiddenFromAT(el)) continue;
      let r = el.getBoundingClientRect();
      if ((el.type === 'checkbox' || el.type === 'radio') && el.labels && el.labels.length) {
        const l = el.labels[0].getBoundingClientRect();
        r = { width: Math.max(r.width, l.width), height: Math.max(r.height, l.height) };
      }
      if (r.width < 23.5 || r.height < 23.5) add('a11y: touch target ' + Math.round(r.width) + 'x' + Math.round(r.height) + ' ' + desc(el));
    }
  }
  return out;
})
