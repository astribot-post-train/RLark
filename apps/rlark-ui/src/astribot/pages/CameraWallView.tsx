import { useLayoutEffect, useRef, useState } from "react";
import { cameraMediaScheduler } from "../lib/cameraMediaScheduler";
import {
  cameraCells,
  DEFAULT_CAMERA_FPS,
  publishedCameras,
  type CameraFps,
  type CameraView,
} from "../lib/cameras";
import type { FleetStation, StationFleetMeta } from "../lib/stationFleet";
import {
  stationPhase,
  stationPhaseLabel,
  stationStatusClass,
} from "../lib/stationDisplay";
import { CameraWall } from "./StationCard";
import { fleetAlertLabel, fleetKey } from "./StationsTableView";

export function FleetPagination({
  page,
  pageCount,
  zh,
  onPage,
}: {
  page: number;
  pageCount: number;
  zh: boolean;
  onPage: (page: number) => void;
}) {
  return (
    <div className="stations-pagination">
      <button
        type="button"
        className="secondary-button"
        disabled={page <= 1}
        onClick={() => onPage(page - 1)}
      >
        {zh ? "上一页" : "Previous"}
      </button>
      <span aria-live="polite">
        {page} / {pageCount}
      </span>
      <button
        type="button"
        className="secondary-button"
        disabled={page >= pageCount}
        onClick={() => onPage(page + 1)}
      >
        {zh ? "下一页" : "Next"}
      </button>
    </div>
  );
}

type WallFit = {
  wallW: number;
  pictureH: number;
  gridH: number;
  fits: boolean;
};

// The scrollport is the stations page, not the window. 100dvh still counts the
// top bar and the platform footer, which is how a wide 16:9 grid lost its
// second row under the fold while a fixed picture cap left the wall narrow.
function pageScrollport(wall: HTMLElement): HTMLElement {
  return wall.closest<HTMLElement>(".page-content") ?? document.documentElement;
}

function measureWall(wall: HTMLElement, columns: 2 | 3): WallFit | null {
  const grid = wall.querySelector<HTMLElement>(".stations-wall-grid");
  if (!grid) return null;
  const scrollport = pageScrollport(wall);
  const portRect = scrollport.getBoundingClientRect();
  const gridRect = grid.getBoundingClientRect();
  const gridTop = gridRect.top - portRect.top + scrollport.scrollTop;
  const frame = wall.closest<HTMLElement>(".stations-frame");
  const frameStyle = frame ? getComputedStyle(frame) : undefined;
  const frameGap = frameStyle ? Number.parseFloat(frameStyle.rowGap || frameStyle.gap) || 0 : 0;
  const admission = frame?.querySelector<HTMLElement>(".stations-admission-section");
  const admissionH = admission?.offsetHeight ?? 0;
  const admissionMargin = admission ? Number.parseFloat(getComputedStyle(admission).marginTop) || 0 : 0;
  const sample = grid.querySelector<HTMLElement>(".stations-wall-tile");
  const picture = sample?.querySelector<HTMLElement>(".station-tile-main");
  const extra = sample && picture ? Math.max(0, sample.offsetHeight - picture.offsetHeight) : 28;
  const gridStyle = getComputedStyle(grid);
  const gap = Number.parseFloat(gridStyle.columnGap || gridStyle.gap) || 8;
  const host = wall.parentElement ?? wall;
  const portStyle = getComputedStyle(scrollport);
  const padBottom = Number.parseFloat(portStyle.paddingBottom) || 0;
  // Full content width. The picture stays 16:9 inside the cell (object-fit: contain).
  // When that frame is shorter than the cell, the cell letterboxes instead of
  // shrinking the whole wall into a centered strip or pushing the next row away.
  const availableW = Math.max(1, host.clientWidth);
  const naturalPicture = ((availableW - (columns - 1) * gap) / columns) * (9 / 16);
  const naturalGrid = columns * (naturalPicture + extra) + (columns - 1) * gap;
  const budget = scrollport.clientHeight - gridTop - padBottom - admissionH - admissionMargin - frameGap - 2;
  const minPicture = columns === 3 ? 72 : 88;
  const minGrid = columns * (minPicture + extra) + (columns - 1) * gap;
  const fits = budget >= minGrid;
  const gridH = fits ? Math.min(naturalGrid, Math.max(minGrid, budget)) : minGrid;
  const pictureH = (gridH - (columns - 1) * gap) / columns - extra;
  return {
    wallW: Math.round(availableW),
    pictureH: Math.max(1, Math.round(pictureH)),
    gridH: Math.max(1, Math.round(gridH)),
    fits,
  };
}

function WallTile({
  item,
  meta,
  zh,
  onPreview,
}: {
  item: FleetStation;
  meta: StationFleetMeta;
  zh: boolean;
  onPreview: (
    item: FleetStation,
    view: CameraView,
    trigger: HTMLElement,
    fps?: CameraFps,
  ) => void;
}) {
  const measuredFps = useRef<HTMLSpanElement>(null);
  const phase = stationPhase(item);
  const cells = cameraCells(
    publishedCameras(
      item.site?.cameras ?? item.annotations?.["rlark.io/cameras"],
    ),
    zh,
  );
  const alert = fleetAlertLabel(meta, zh);
  return (
    <article className="stations-wall-tile" data-severity={meta.alertSeverity}>
      <CameraWall
        cells={cells}
        node={item.metadata.name}
        namespace={item.metadata.namespace}
        zh={zh}
        online={phase === "Bound"}
        fps={DEFAULT_CAMERA_FPS}
        measuredFps={measuredFps}
        onZoom={(view, trigger, fps) => onPreview(item, view, trigger, fps)}
        monitor
      />
      <footer>
        <strong title={item.metadata.name}>{item.metadata.name}</strong>
        <span className={`status ${stationStatusClass(phase)}`}>
          <i />
          {stationPhaseLabel(phase, zh)}
        </span>
        {alert && <span className="stations-table-alert">{alert}</span>}
      </footer>
    </article>
  );
}

export function CameraWallView({
  items,
  metaByKey,
  zh,
  onPreview,
  onClear,
  selected,
}: {
  items: FleetStation[];
  metaByKey: ReadonlyMap<string, StationFleetMeta>;
  zh: boolean;
  selected: boolean;
  onClear: () => void;
  onPreview: (
    item: FleetStation,
    view: CameraView,
    trigger: HTMLElement,
    fps?: CameraFps,
  ) => void;
}) {
  const [columns, setColumns] = useState<2 | 3>(2);
  const [page, setPage] = useState(1);
  const [fit, setFit] = useState<WallFit>();
  const wallRef = useRef<HTMLElement>(null);
  const size = columns * columns;
  const pageCount = Math.max(1, Math.ceil(items.length / size));
  const currentPage = Math.min(page, pageCount);
  useLayoutEffect(() => {
    cameraMediaScheduler.setMaxLiveStreams(size);
    return () => cameraMediaScheduler.setMaxLiveStreams(3);
  }, [size]);
  useLayoutEffect(() => {
    const wall = wallRef.current;
    if (!wall) return;
    let frame = 0;
    const apply = () => {
      const next = measureWall(wall, columns);
      if (!next) return;
      setFit((current) =>
        current
        && current.wallW === next.wallW
        && current.pictureH === next.pictureH
        && current.gridH === next.gridH
        && current.fits === next.fits
          ? current
          : next,
      );
    };
    apply();
    const scrollport = pageScrollport(wall);
    const observer = new ResizeObserver(() => {
      window.cancelAnimationFrame(frame);
      frame = window.requestAnimationFrame(apply);
    });
    observer.observe(scrollport);
    observer.observe(wall);
    return () => {
      window.cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, [columns, items.length, currentPage]);

  return (
    <section
      ref={wallRef}
      className="stations-wall"
      aria-label={zh ? "监控墙" : "Camera wall"}
      data-wall-fits={fit ? String(fit.fits) : undefined}
    >
      <div className="stations-view-options">
        <div
          className="stations-segments"
          role="group"
          aria-label={zh ? "宫格大小" : "Grid size"}
        >
          {([2, 3] as const).map((value) => (
            <button
              key={value}
              type="button"
              aria-pressed={columns === value}
              onClick={() => {
                setColumns(value);
                setPage(1);
              }}
            >
              {value} × {value}
            </button>
          ))}
        </div>
        {columns === 3 && (
          <small className="muted">
            {zh
              ? "HTTP/1.1 浏览器连接上限可能让部分画面排队。"
              : "The browser’s HTTP/1.1 connection limit may queue some streams."}
          </small>
        )}
        {selected && (
          <button
            type="button"
            className="secondary-button"
            onClick={() => {
              onClear();
              setPage(1);
            }}
          >
            {zh ? "显示全部机器人" : "Show all robots"}
          </button>
        )}
        <FleetPagination
          page={currentPage}
          pageCount={pageCount}
          zh={zh}
          onPage={setPage}
        />
      </div>
      {items.length ? (
        <div
          className={`stations-wall-grid is-${columns}`}
          data-wall-width={fit?.wallW}
          data-wall-picture-h={fit?.pictureH}
          style={fit ? { width: "100%", height: fit.gridH } : { width: "100%" }}
        >
          {items
            .slice((currentPage - 1) * size, currentPage * size)
            .map((item) => {
              const key = fleetKey(item);
              return (
                <WallTile
                  key={key}
                  item={item}
                  meta={metaByKey.get(key)!}
                  zh={zh}
                  onPreview={onPreview}
                />
              );
            })}
        </div>
      ) : (
        <div className="stations-state muted">
          {zh ? "没有匹配的机器人" : "No matching robots"}
        </div>
      )}
    </section>
  );
}
