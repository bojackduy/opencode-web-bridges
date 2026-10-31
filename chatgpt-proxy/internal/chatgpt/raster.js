// Rasterize substantive inline SVGs of one assistant answer to PNG data URLs.
// Evaluate as an async function of the answer's DOM identity (the lastID from
// responseProbe). Best-effort: any failure yields fewer (or no) images, never
// an exception to the caller. The text serializer keeps its "[SVG: ...]"
// fallback, so meaning survives even when rasterization cannot run.
(async (lastID) => {
  const out = [];
  try {
    if (typeof lastID !== 'string' || !lastID || !/^[a-zA-Z0-9-]+$/.test(lastID)) return out;
    const ownerID = e => (e?.getAttribute('data-chatgpt-search-message-ids') || e?.getAttribute('data-message-id') || '').trim().split(/\s+/).at(-1);
    const dilRoot = [...document.querySelectorAll('[data-dil-message-id]')].find(e => e.getAttribute('data-dil-message-id') === lastID) || null;
    const heading = [...document.querySelectorAll('h4')].filter(e => e.getAttribute('data-conversation-role') === 'assistant' || (e.textContent || '').trim() === 'ChatGPT said:').find(e => ownerID(e.closest('[data-chatgpt-search-message-ids], [data-message-id]')) === lastID) || null;
    const body = (heading && heading.nextElementSibling) || dilRoot;
    if (!body || !body.isConnected) return out;
    const svgs = [...body.querySelectorAll('svg')].filter(substantive).slice(0, 4);
    for (const svg of svgs) {
      try {
        const dataURL = await rasterizeOne(svg);
        if (dataURL) out.push({ alt: describe(svg), dataURL });
      } catch (_) {}
    }
  } catch (_) {}
  return out;

  // Only a substantive <svg> is an answer diagram. ChatGPT UI chrome ships
  // 16-24px icon SVGs (copy/thumbs/action icons) that are not always wrapped in
  // a button or marked aria-hidden, so rasterizing the body's SVGs unfiltered
  // produced near-blank 40x40 PNGs (a 20px icon at 2x) instead of the diagram.
  // Skip chrome, semantic-row decorative icons, and icon-sized glyphs; a real
  // diagram is unitless-with-viewBox or hundreds of px.
  function chromeSvg(e) {
    return !!e.closest('button, [role="toolbar"], [hidden], [aria-hidden="true"], script, style, [data-markdown-copy="exclude"], [data-testid="thought-block"], [data-thinking], [data-d-component="badge"]');
  }
  function autoDim(v) {
    const m = /^\s*([\d.]+)\s*(px|em|rem|%)?\s*$/.exec(v || '');
    if (!m) return null;
    const n = parseFloat(m[1]);
    return (m[2] === 'em' || m[2] === 'rem') ? n * 16 : n;
  }
  function iconSized(e) {
    const w = autoDim(e.getAttribute('width')), h = autoDim(e.getAttribute('height'));
    return w !== null && h !== null && w > 0 && h > 0 && Math.max(w, h) <= 24;
  }
  function renderedSmall(e) {
    try { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && Math.max(r.width, r.height) <= 24; } catch (_) { return false; }
  }
  function decorative(e) {
    return e.matches('[data-d-component="icon"]') && e.closest('[data-d-component="row"]') &&
      !e.hasAttribute('aria-label') && !e.hasAttribute('aria-labelledby') && !e.querySelector('title, desc, text');
  }
  function substantive(e) {
    return !chromeSvg(e) && !iconSized(e) && !renderedSmall(e) && !decorative(e);
  }

  function describe(svg) {
    const labelled = (svg.getAttribute('aria-label') || '').trim();
    if (labelled) return labelled.slice(0, 120);
    const byIds = (svg.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean)
      .map(id => { try { return (document.getElementById(id)?.textContent || '').trim(); } catch (_) { return ''; } })
      .filter(Boolean).join(' ');
    if (byIds) return byIds.slice(0, 120);
    const inner = [...svg.querySelectorAll('title, desc, text')].map(n => (n.textContent || '').trim()).filter(Boolean).join('; ');
    return (inner || 'diagram').slice(0, 120);
  }

  async function rasterizeOne(svg) {
    const clone = svg.cloneNode(true);
    clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
    let w = parseFloat(clone.getAttribute('width') || '');
    let h = parseFloat(clone.getAttribute('height') || '');
    const vb = (clone.getAttribute('viewBox') || '').trim().split(/[\s,]+/).map(Number);
    if ((!isFinite(w) || w <= 0 || !isFinite(h) || h <= 0) && vb.length === 4 && vb.every(isFinite) && vb[2] > 0 && vb[3] > 0) { w = vb[2]; h = vb[3]; }
    if (!isFinite(w) || w <= 0 || !isFinite(h) || h <= 0) {
      try { const r = svg.getBoundingClientRect(); if (r.width > 0 && r.height > 0) { w = r.width; h = r.height; } } catch (_) {}
    }
    if (!isFinite(w) || w <= 0 || !isFinite(h) || h <= 0) { w = 800; h = 600; }
    // Cap output pixels; upscale small diagrams so labels stay legible.
    const maxW = 1200, maxH = 1200;
    let scale = Math.min(2, maxW / w, maxH / h);
    if (!isFinite(scale) || scale <= 0) scale = 1;
    const W = Math.max(1, Math.round(w * scale)), H = Math.max(1, Math.round(h * scale));
    clone.setAttribute('width', String(W));
    clone.setAttribute('height', String(H));
    // Opaque white ground: page SVGs assume a light canvas, and a transparent
    // PNG over a dark terminal would hide dark strokes.
    const bg = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    bg.setAttribute('width', '100%'); bg.setAttribute('height', '100%'); bg.setAttribute('fill', 'white');
    clone.insertBefore(bg, clone.firstChild);
    const str = new XMLSerializer().serializeToString(clone);
    if (!str || str.length > 1500000) return '';
    const url = URL.createObjectURL(new Blob([str], { type: 'image/svg+xml;charset=utf-8' }));
    try {
      const dataURL = await new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('svg raster timeout')), 8000);
        const image = new Image();
        image.onload = () => {
          clearTimeout(timer);
          try {
            const canvas = document.createElement('canvas');
            canvas.width = W; canvas.height = H;
            const ctx = canvas.getContext('2d');
            if (!ctx) { reject(new Error('no 2d context')); return; }
            ctx.fillStyle = 'white'; ctx.fillRect(0, 0, W, H);
            ctx.drawImage(image, 0, 0, W, H);
            resolve(canvas.toDataURL('image/png'));
          } catch (e) { reject(e); }
        };
        image.onerror = () => { clearTimeout(timer); reject(new Error('svg load failed')); };
        image.src = url;
      });
      return (typeof dataURL === 'string' && dataURL.startsWith('data:image/png;base64,')) ? dataURL : '';
    } finally {
      URL.revokeObjectURL(url);
    }
  }
})
