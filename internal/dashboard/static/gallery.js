// Gallery kiểu justified cho PhotoVault.
//
// - Bố cục: port thuật toán xếp hàng của Justified Gallery (row-layout.ts):
//   dồn ảnh vào hàng tới khi đủ rộng, rồi co chiều cao hàng để lấp kín chiều ngang.
// - Cuộn vô hạn: tải từng trang qua /api/photos (phân trang keyset).
// - Ảo hoá: chỉ giữ trên DOM các hàng gần màn hình; hàng ra xa bị gỡ (trình duyệt
//   giải phóng ảnh). Nhờ vậy 20k ảnh không làm nặng trình duyệt.
(() => {
  'use strict';

  const root = document.getElementById('gallery');
  if (!root) return;

  const PAGE = 150; // số ảnh mỗi lần tải
  const MARGIN = 4; // khoảng cách giữa ảnh (px)
  const MONTH_H = 44; // chiều cao tiêu đề tháng (px)
  const JUSTIFY_THRESHOLD = 0.9; // hàng cuối đủ ≥90% chiều ngang thì vẫn kéo đều

  const dateFilter = root.dataset.date || '';
  const statusEl = document.getElementById('gallery-status');
  const header = document.querySelector('header');
  const canvas = document.createElement('div');
  canvas.className = 'jg-canvas';
  root.appendChild(canvas);

  const items = [];
  let nextCursor = '';
  let done = false;
  let loading = false;
  let retryAt = 0;
  let layout = { blocks: [], height: 0, width: 0 };
  const live = new Map(); // khoá → phần tử đang có trên DOM

  const rowHeightFor = (w) => (w < 600 ? 120 : w < 1100 ? 180 : 230);
  const ratioOf = (it) => (it.w > 0 && it.h > 0 ? it.w / it.h : 1);
  const thumbURL = (it) => '/thumb?path=' + encodeURIComponent(it.path);
  const mediaURL = (it) => '/media?path=' + encodeURIComponent(it.path);
  const extOf = (name) => {
    const i = name.lastIndexOf('.');
    return i >= 0 ? name.slice(i + 1).toUpperCase() : '';
  };
  const formatSize = (kb) => (kb >= 1024 ? (kb / 1024).toFixed(1) + ' MB' : kb + ' KB');
  const monthLabel = (key) => {
    const [y, m] = key.split('-');
    return 'Tháng ' + Number(m) + ', ' + y;
  };
  const rootTop = () => root.getBoundingClientRect().top + window.scrollY;
  const headerH = () => (header ? header.offsetHeight : 0);
  const setStatus = (text) => {
    statusEl.textContent = text;
  };

  // ---- Bố cục ----

  // Dựng toàn bộ bố cục cho các ảnh đã tải: danh sách "khối" theo thứ tự dọc,
  // mỗi khối là tiêu đề tháng hoặc một hàng ảnh. Chỉ là phép tính số học, 20k
  // ảnh vẫn chạy trong vài mili-giây.
  function computeLayout(width) {
    const rh = rowHeightFor(width);
    const maxRH = rh * 1.5;
    const blocks = [];
    let y = 0;
    let row = [];
    let rowAR = 0;
    let month = null;

    function flush(isLast) {
      if (!row.length) return;
      const avail = width - MARGIN * (row.length - 1);
      let h = avail / rowAR;
      let justify = true;
      // Hàng cuối (của tháng hoặc của phần đã tải) còn thưa: giữ cao chuẩn, căn trái.
      if (isLast && (rowAR * rh) / avail < JUSTIFY_THRESHOLD) {
        h = rh;
        justify = false;
      }
      if (h > maxRH) {
        h = maxRH;
        justify = false;
      }
      h = Math.round(h);
      const tiles = [];
      let x = 0;
      row.forEach((idx, i) => {
        let w = Math.round(ratioOf(items[idx]) * h);
        if (justify && i === row.length - 1) w = width - x; // khử sai số làm tròn
        w = Math.max(1, w);
        tiles.push({ idx, x, w });
        x += w + MARGIN;
      });
      blocks.push({ kind: 'row', top: y, h, tiles });
      y += h + MARGIN;
      row = [];
      rowAR = 0;
    }

    items.forEach((it, idx) => {
      const key = it.taken.slice(0, 7); // "2024-06"
      if (key !== month) {
        flush(true);
        month = key;
        blocks.push({ kind: 'month', top: y, h: MONTH_H, key });
        y += MONTH_H;
      }
      row.push(idx);
      rowAR += ratioOf(it);
      if (rowAR * rh + MARGIN * (row.length - 1) >= width) flush(false);
    });
    flush(true);
    return { blocks, height: y, width };
  }

  // ---- Render (ảo hoá) ----

  // Khối đầu tiên có đáy ≥ y (tìm nhị phân).
  function firstBlockFrom(y) {
    const b = layout.blocks;
    let lo = 0;
    let hi = b.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (b[mid].top + b[mid].h < y) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  function place(el, x, y, w, h) {
    el.style.transform = `translate(${x}px, ${y}px)`;
    el.style.width = w + 'px';
    el.style.height = h + 'px';
  }

  function makeTile(idx) {
    const it = items[idx];
    const a = document.createElement('a');
    a.className = 'jg-tile';
    a.href = mediaURL(it);
    a.target = '_blank';
    a.rel = 'noopener';
    a.dataset.idx = String(idx);
    a.title = it.name;
    if (it.kind === 'image') {
      const img = document.createElement('img');
      img.decoding = 'async';
      img.alt = it.name;
      img.addEventListener('load', () => a.classList.add('loaded'), { once: true });
      img.addEventListener('error', () => a.classList.add('broken'), { once: true });
      img.src = thumbURL(it);
      a.appendChild(img);
    } else {
      const ph = document.createElement('span');
      ph.className = 'jg-ph';
      if (it.kind === 'video') {
        const play = document.createElement('span');
        play.className = 'jg-play';
        play.setAttribute('aria-hidden', 'true');
        play.textContent = '▶';
        ph.appendChild(play);
      }
      const ext = document.createElement('span');
      ext.className = 'jg-ext';
      ext.textContent = extOf(it.name);
      ph.appendChild(ext);
      a.appendChild(ph);
    }
    const cap = document.createElement('span');
    cap.className = 'jg-cap';
    cap.textContent = it.label;
    a.appendChild(cap);
    return a;
  }

  function render() {
    const top = rootTop();
    const buffer = window.innerHeight * 1.5;
    const from = window.scrollY - top - buffer;
    const to = window.scrollY - top + window.innerHeight + buffer;
    const keep = new Set();

    for (let i = firstBlockFrom(from); i < layout.blocks.length && layout.blocks[i].top <= to; i++) {
      const b = layout.blocks[i];
      if (b.kind === 'month') {
        const key = 'm' + b.key;
        keep.add(key);
        let el = live.get(key);
        if (!el) {
          el = document.createElement('h2');
          el.className = 'jg-month';
          el.textContent = monthLabel(b.key);
          canvas.appendChild(el);
          live.set(key, el);
        }
        place(el, 0, b.top, layout.width, b.h);
      } else {
        for (const t of b.tiles) {
          const key = 't' + t.idx;
          keep.add(key);
          let el = live.get(key);
          if (!el) {
            el = makeTile(t.idx);
            canvas.appendChild(el);
            live.set(key, el);
          }
          place(el, t.x, b.top, t.w, b.h);
        }
      }
    }
    // Gỡ phần tử ra xa màn hình → trình duyệt huỷ tải/giải phóng ảnh.
    for (const [key, el] of live) {
      if (!keep.has(key)) {
        el.remove();
        live.delete(key);
      }
    }
  }

  function relayout() {
    const width = root.clientWidth;
    if (!width) return;
    layout = computeLayout(width);
    canvas.style.height = layout.height + 'px';
    render();
  }

  // Chỉ số ảnh đầu tiên đang nhìn thấy (giữ vị trí khi đổi kích thước cửa sổ).
  function topVisibleIdx() {
    const y = window.scrollY - rootTop() + headerH();
    for (let i = firstBlockFrom(y); i < layout.blocks.length; i++) {
      const b = layout.blocks[i];
      if (b.kind === 'row') return b.tiles[0].idx;
    }
    return -1;
  }

  function topOfIdx(idx) {
    for (const b of layout.blocks) {
      if (b.kind === 'row' && b.tiles.some((t) => t.idx === idx)) return b.top;
    }
    return null;
  }

  // ---- Tải dữ liệu ----

  const emptyText = () =>
    dateFilter
      ? 'Không có ảnh nào trong ngày này.'
      : 'Chưa có ảnh nào. Bật Auto Upload trên app Nextcloud để bắt đầu backup.';

  async function loadMore() {
    if (loading || done || Date.now() < retryAt) return;
    loading = true;
    setStatus('Đang tải…');
    try {
      const qs = new URLSearchParams({ limit: String(PAGE) });
      if (nextCursor) qs.set('cursor', nextCursor);
      if (dateFilter) qs.set('date', dateFilter);
      const res = await fetch('/api/photos?' + qs.toString(), {
        headers: { Accept: 'application/json' },
      });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      items.push(...data.items);
      nextCursor = data.next || '';
      done = !nextCursor;
      relayout();
      setStatus(done ? (items.length ? `Đã hiện hết ${items.length} mục.` : emptyText()) : '');
    } catch (err) {
      retryAt = Date.now() + 3000;
      setStatus('Không tải được ảnh (' + err.message + '). Đang thử lại…');
      setTimeout(maybeLoadMore, 3100);
    } finally {
      loading = false;
    }
    // Nội dung chưa lấp đầy màn hình → tải tiếp.
    if (!done) requestAnimationFrame(maybeLoadMore);
  }

  function maybeLoadMore() {
    if (done || loading) return;
    const bottom = rootTop() + layout.height;
    if (window.scrollY + window.innerHeight * 3 >= bottom) loadMore();
  }

  // ---- Sự kiện ----

  let ticking = false;
  window.addEventListener(
    'scroll',
    () => {
      if (ticking) return;
      ticking = true;
      requestAnimationFrame(() => {
        ticking = false;
        render();
        maybeLoadMore();
      });
    },
    { passive: true }
  );

  let lastWidth = 0;
  new ResizeObserver(() => {
    const w = root.clientWidth;
    if (w === lastWidth) return;
    lastWidth = w;
    const anchor = topVisibleIdx();
    relayout();
    if (anchor >= 0) {
      const y = topOfIdx(anchor);
      if (y !== null) window.scrollTo(0, rootTop() + y - headerH());
    }
  }).observe(root);

  canvas.addEventListener('click', (e) => {
    const a = e.target.closest('.jg-tile');
    if (!a) return;
    // Ctrl/Cmd/Shift-click hoặc chuột giữa: để trình duyệt mở tab mới như link thường.
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    openLightbox(Number(a.dataset.idx));
  });

  // ---- Lightbox ----

  const lb = document.getElementById('lb');
  const lbStage = lb.querySelector('.lb-stage');
  const lbCap = lb.querySelector('.lb-cap');
  const lbOpen = lb.querySelector('.lb-open');
  const lbPrev = lb.querySelector('.lb-prev');
  const lbNext = lb.querySelector('.lb-next');
  const lbClose = lb.querySelector('.lb-close');
  let lbIdx = -1;
  let lastFocus = null;

  function unsupported(it) {
    const msg = document.createElement('p');
    msg.className = 'lb-msg';
    msg.textContent =
      'Trình duyệt không xem trước được định dạng ' + extOf(it.name) + '. Bấm "Mở file gốc" để tải về.';
    return msg;
  }

  function show(idx) {
    if (idx < 0 || idx >= items.length) return;
    lbIdx = idx;
    const it = items[idx];
    let el;
    if (it.kind === 'video') {
      el = document.createElement('video');
      el.controls = true;
      el.autoplay = true;
      el.playsInline = true;
      el.preload = 'metadata';
      el.src = mediaURL(it);
    } else {
      el = document.createElement('img');
      el.alt = it.name;
      el.decoding = 'async';
      if (it.kind === 'image') {
        // Hiện thumbnail (đã cache) ngay, rồi thay bằng ảnh gốc khi tải xong.
        el.src = thumbURL(it);
        const full = new Image();
        full.addEventListener('load', () => {
          if (lbIdx === idx) el.src = full.src;
        });
        full.src = mediaURL(it);
      } else {
        // HEIC...: Safari xem được, trình duyệt khác thì báo không hỗ trợ.
        el.addEventListener('error', () => el.replaceWith(unsupported(it)), { once: true });
        el.src = mediaURL(it);
      }
    }
    lbStage.replaceChildren(el);
    lbCap.textContent = it.name + ' · ' + it.label + ' · ' + formatSize(it.sizeKB);
    lbOpen.href = mediaURL(it);
    lbPrev.disabled = idx === 0;
    lbNext.disabled = idx === items.length - 1 && done;
    if (idx >= items.length - 5) loadMore(); // sắp hết phần đã tải → tải trước
  }

  function openLightbox(idx) {
    lastFocus = document.activeElement;
    lb.hidden = false;
    document.body.classList.add('lb-active');
    show(idx);
    lbClose.focus();
  }

  function closeLightbox() {
    lb.hidden = true;
    document.body.classList.remove('lb-active');
    lbStage.replaceChildren(); // gỡ video → dừng phát
    lbIdx = -1;
    if (lastFocus && document.contains(lastFocus)) lastFocus.focus();
  }

  lbClose.addEventListener('click', closeLightbox);
  lbPrev.addEventListener('click', () => show(lbIdx - 1));
  lbNext.addEventListener('click', () => show(lbIdx + 1));
  lb.addEventListener('click', (e) => {
    if (e.target === lb || e.target === lbStage) closeLightbox();
  });
  document.addEventListener('keydown', (e) => {
    if (lb.hidden) return;
    if (e.key === 'Escape') closeLightbox();
    else if (e.key === 'ArrowLeft') show(lbIdx - 1);
    else if (e.key === 'ArrowRight') show(lbIdx + 1);
  });

  relayout();
  loadMore();
})();
