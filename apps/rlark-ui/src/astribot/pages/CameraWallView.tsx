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
  videoW: number;
  gridW: number;
  fits: boolean;
};

// The scrollport is the stations page, not the window. The picture is a real
// 16:9 box; leftover content width stays page background.
function pageScrollport(wall: HTMLElement): HTMLElement {
  return wall.closest<HTMLElement>(".page-content") ?? document.documentElement;
}

// videoWidth = min(widthFit, heightFit * 16/9), with the grid centered on that width.
export function wallFrameSize(input: {
  contentWidth: number;
  contentHeight: number;
  padX: number;
  headerH: number;
  statusH: number;
  gap: number;
  columns: 2 | 3;
}): { videoW: number; gridW: number } {
  const { contentWidth, contentHeight, padX, headerH, statusH, gap, columns } =
    input;
  const rows = columns;
  const widthFit = (contentWidth - padX - (columns - 1) * gap) / columns;
  const heightFit =
    (contentHeight - headerH - (rows - 1) * gap) / rows - statusH;
  let videoW = Math.floor(
    Math.min(widthFit, (Math.max(1, heightFit) * 16) / 9),
  );
  const innerW = contentWidth - padX;
  const fitsFrame = (width: number) => {
    const pictureH = (width * 9) / 16;
    const gridH = rows * (pictureH + statusH) + (rows - 1) * gap;
    const gridW = columns * width + (columns - 1) * gap;
    return gridH <= contentHeight - headerH + 0.5 && gridW <= innerW + 0.5;
  };
  while (videoW > 1 && !fitsFrame(videoW)) videoW -= 1;
  return {
    videoW: Math.max(1, videoW),
    gridW: columns * Math.max(1, videoW) + (columns - 1) * gap,
  };
}

function measureWall(wall: HTMLElement, columns: 2 | 3): WallFit | null {
  const grid = wall.querySelector<HTMLElement>(".stations-wall-grid");
  if (!grid) return null;
  const scrollport = pageScrollport(wall);
  const frame = wall.closest<HTMLElement>(".stations-frame");
  const filters = frame?.querySelector<HTMLElement>(".stations-fleet-filters");
  const footer = document.querySelector<HTMLElement>(".platform-footer");
  const header = wall.querySelector<HTMLElement>(".stations-view-options");
  if (!filters || !footer) return null;
  const portRect = scrollport.getBoundingClientRect();
  const portStyle = getComputedStyle(scrollport);
  const padLeft = Number.parseFloat(portStyle.paddingLeft) || 0;
  const padRight = Number.parseFloat(portStyle.paddingRight) || 0;
  const padBottom = Number.parseFloat(portStyle.paddingBottom) || 0;
  const contentBottom = Math.min(
    footer.getBoundingClientRect().top,
    portRect.bottom,
  );
  const contentTop = Math.max(
    filters.getBoundingClientRect().bottom,
    wall.getBoundingClientRect().top,
  );
  const contentHeight = Math.max(0, contentBottom - contentTop - padBottom);
  const contentWidth = scrollport.clientWidth;
  const headerStyle = header ? getComputedStyle(header) : undefined;
  const headerH = header
    ? header.offsetHeight +
      (Number.parseFloat(headerStyle?.marginTop || "") || 0) +
      (Number.parseFloat(headerStyle?.marginBottom || "") || 0)
    : 0;
  const status = grid.querySelector<HTMLElement>(".stations-wall-tile footer");
  const statusH = status?.offsetHeight || 22;
  const gridStyle = getComputedStyle(grid);
  const gap = Number.parseFloat(gridStyle.columnGap || gridStyle.gap) || 14;
  const frameSize = wallFrameSize({
    contentWidth,
    contentHeight,
    padX: padLeft + padRight,
    headerH,
    statusH,
    gap,
    columns,
  });
  const minPicture = columns === 3 ? 72 : 88;
  return {
    videoW: frameSize.videoW,
    gridW: frameSize.gridW,
    fits: (frameSize.videoW * 9) / 16 >= minPicture,
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
        current &&
        current.videoW === next.videoW &&
        current.gridW === next.gridW &&
        current.fits === next.fits
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
      data-wall-video-w={fit?.videoW}
      style={
        fit ? { ["--wall-video" as string]: `${fit.videoW}px` } : undefined
      }
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
          data-wall-video-w={fit?.videoW}
          data-wall-grid-w={fit?.gridW}
          style={fit ? { width: fit.gridW } : undefined}
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
