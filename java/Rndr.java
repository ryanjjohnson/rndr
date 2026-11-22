import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileDescriptor;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Locale;

/**
 * rndr - ASCII STL viewer for the terminal.
 *
 * Plain Java 8, no libraries. The only external program used is stty, and only
 * for the interactive mode (raw keyboard input + terminal size).
 */
public final class Rndr {

    static final class Fail extends RuntimeException {
        Fail(String m) { super(m); }
    }

    // Each character cell is sampled SX x SY times; with the usual 1:2 cell
    // shape that makes the samples square.
    static final int SX = 2, SY = 4;
    static final String[] MODES = {"both", "wire", "mesh", "xray", "shade"};
    static final int BOTH = 0, WIRE = 1, MESH = 2, XRAY = 3, SHADE = 4;
    static final String RAMP = ".,:;=+*#%@";
    static final double DEF_RX = 25, DEF_RY = -30, DEF_RZ = 0;
    static final double ROT_STEP = 5, PAN_STEP = 0.1, ZOOM_STEP = 1.15;

    // ---- options -----------------------------------------------------------
    static double aspect = 2.0;      // character cell height / width
    static double crease = 30;       // feature-edge angle in degrees
    static boolean invert;

    // ---- mesh --------------------------------------------------------------
    static String fileName = "";
    static int nv, nt, ne;
    static float[] vx, vy, vz;       // welded vertices, normalised to a unit sphere
    static int[] tri;                // 3 vertex indices per triangle
    static float[] fnx, fny, fnz;    // face normals
    static int[] ea, eb, ef1, ef2;   // unique edges and the faces on either side
    static byte[] eflag;             // 1 = direction of first use, 2 = winding mismatch, 4 = feature
    static double dimX, dimY, dimZ;  // original bounding box size

    // ---- view --------------------------------------------------------------
    static double rotX = DEF_RX, rotY = DEF_RY, rotZ = DEF_RZ;   // degrees
    static double[] R = euler(rotX, rotY, rotZ);
    static double zoom = 1, panX = 0, panY = 0;
    static int mode = BOTH;

    public static void main(String[] argv) {
        try {
            run(argv);
        } catch (Fail | IOException e) {
            System.err.println("rndr: " + e.getMessage());
            System.exit(1);
        } catch (OutOfMemoryError e) {
            System.err.println("rndr: out of memory - try RNDR_JAVA_OPTS=-Xmx4g");
            System.exit(1);
        }
    }

    static final String USAGE =
        "usage: rndr FILE.stl                      interactive viewer\n"
      + "       rndr FILE.stl RX RY RZ [ZOOM [PANX PANY]]   print one frame to stdout\n"
      + "\n"
      + "  RX RY RZ   tilt, spin, roll: degrees about the screen axes (X right, Y up, Z toward you),\n"
      + "             applied Z first, then Y, then X. 0 0 0 is the front view with model Z up.\n"
      + "  ZOOM       1.0 fits the whole model in the frame\n"
      + "  PANX PANY  shift, in units of the model's radius\n"
      + "  FILE       may be - to read the STL from stdin (print mode only)\n"
      + "\n"
      + "options:\n"
      + "  -p, --print          print one frame of the default view instead of going interactive\n"
      + "  -i, --interactive    go interactive even when a view is given\n"
      + "  -s, --size COLSxROWS frame size (default: the terminal, or 80x24)\n"
      + "             COLS      just a width: as many rows as the model needs, no blank rows\n"
      + "  -m, --mode MODE      both (shaded + edges, default), wire (edges, hidden lines removed),\n"
      + "                       mesh (every triangle), xray (see-through mesh), shade\n"
      + "  -o, --out FILE       write the frame to FILE instead of stdout\n"
      + "      --crease DEG     angle at which an edge counts as a feature edge (default 30)\n"
      + "      --aspect N       height/width of a character cell (default 2.0)\n"
      + "      --invert         reverse the shading ramp (for dark-on-light output)\n"
      + "\n"
      + "interactive keys:\n"
      + "  w s       tilt            a d   spin           z x   roll   (arrows work too)\n"
      + "  W A S D   pan             i o   zoom in / out\n"
      + "  m         cycle mode      r     reset view\n"
      + "  p         save the frame to FILE.rndr.txt in the current directory\n"
      + "  ?         key help        q     quit\n";

    static void run(String[] argv) throws IOException {
        String file = null, out = null, size = null;
        boolean forcePrint = false, forceInteractive = false;
        List<Double> nums = new ArrayList<Double>();
        for (int i = 0; i < argv.length; i++) {
            String a = argv[i];
            Double num = file == null ? null : parseNum(a);
            if (num != null) {
                nums.add(num);
            } else if (a.equals("-h") || a.equals("--help")) {
                System.out.print(USAGE);
                return;
            } else if (a.equals("-p") || a.equals("--print")) {
                forcePrint = true;
            } else if (a.equals("-i") || a.equals("--interactive")) {
                forceInteractive = true;
            } else if (a.equals("--invert")) {
                invert = true;
            } else if (a.equals("-s") || a.equals("--size")) {
                size = need(argv, ++i, a);
            } else if (a.equals("-o") || a.equals("--out")) {
                out = need(argv, ++i, a);
            } else if (a.equals("-m") || a.equals("--mode")) {
                String m = need(argv, ++i, a);
                mode = Arrays.asList(MODES).indexOf(m);
                if (mode < 0) throw new Fail("unknown mode '" + m + "' (both, wire, mesh, xray, shade)");
            } else if (a.equals("--crease")) {
                crease = needNum(argv, ++i, a);
            } else if (a.equals("--aspect")) {
                aspect = needNum(argv, ++i, a);
                if (!(aspect > 0.1 && aspect < 10)) throw new Fail("--aspect out of range");
            } else if (a.startsWith("-") && a.length() > 1) {
                throw new Fail("unknown option " + a + " (try --help)");
            } else if (file == null) {
                file = a;
            } else {
                throw new Fail("expected a number, got '" + a + "' (try --help)");
            }
        }
        if (file == null) {
            System.err.print(USAGE);
            System.exit(2);
        }
        if (nums.size() != 0 && nums.size() != 3 && nums.size() != 4 && nums.size() != 6)
            throw new Fail("expected RX RY RZ [ZOOM [PANX PANY]]");
        if (nums.size() >= 3) { rotX = nums.get(0); rotY = nums.get(1); rotZ = nums.get(2); }
        R = euler(rotX, rotY, rotZ);
        if (nums.size() >= 4) zoom = nums.get(3);
        if (nums.size() >= 6) { panX = nums.get(4); panY = nums.get(5); }
        if (!(zoom > 0)) throw new Fail("zoom must be greater than 0");

        boolean tty = stdoutIsTty();
        boolean interactive = forceInteractive
            || (nums.isEmpty() && !forcePrint && out == null && tty && !file.equals("-"));
        if (interactive && file.equals("-")) throw new Fail("reading from stdin only works in print mode");

        byte[] data;
        if (file.equals("-")) {
            data = readAll(System.in);
            fileName = "stdin";
        } else {
            File f = new File(file);
            if (!f.isFile()) throw new Fail(file + ": no such file");
            fileName = f.getName();
            try {
                data = Files.readAllBytes(f.toPath());
            } catch (IOException e) {
                throw new Fail(file + ": " + e.getMessage());
            }
        }
        load(data);
        data = null;

        if (interactive) {
            interactive();
            return;
        }

        int cols, rows;
        boolean cropRows = false;
        if (size != null) {
            // COLSxROWS, or just COLS (or xROWS): the other side is sized to fit the model
            String[] p = size.toLowerCase(Locale.ROOT).split("x", -1);
            try {
                cols = p[0].trim().isEmpty() ? 0 : Integer.parseInt(p[0].trim());
                rows = p.length < 2 || p[1].trim().isEmpty() ? 0 : Integer.parseInt(p[1].trim());
                if (p.length > 2 || cols < 0 || rows < 0 || (cols == 0 && rows == 0)) throw new NumberFormatException();
            } catch (RuntimeException e) {
                throw new Fail("--size wants COLSxROWS or just COLS, e.g. 100x40 or 100");
            }
            // a visually square frame is exactly what zoom 1.0 fills
            if (rows == 0) { rows = (int) Math.ceil(cols / aspect); cropRows = true; }
            if (cols == 0) cols = (int) Math.ceil(rows * aspect);
        } else {
            int[] sz = defaultSize(tty && out == null);
            cols = sz[0];
            rows = sz[1];
        }
        if (cols < 2 || rows < 2 || (long) cols * rows > 4000000L) throw new Fail("unreasonable frame size");
        String frame = frameText(cols, rows);
        if (cropRows) frame = frame.replaceFirst("^\\n+", "").replaceFirst("\\n+$", "\n");
        byte[] text = frame.getBytes(StandardCharsets.US_ASCII);
        if (out != null) {
            try (OutputStream os = new FileOutputStream(out)) {
                os.write(text);
            } catch (IOException e) {
                throw new Fail(out + ": " + e.getMessage());
            }
        } else {
            System.out.write(text);
            System.out.flush();
        }
    }

    static String need(String[] argv, int i, String opt) {
        if (i >= argv.length) throw new Fail(opt + " needs a value");
        return argv[i];
    }

    static double needNum(String[] argv, int i, String opt) {
        Double d = parseNum(need(argv, i, opt));
        if (d == null) throw new Fail(opt + " needs a number");
        return d;
    }

    static Double parseNum(String s) {
        if (s.isEmpty() || "+-.0123456789".indexOf(s.charAt(0)) < 0) return null;
        try {
            double d = Double.parseDouble(s);
            return Double.isNaN(d) || Double.isInfinite(d) ? null : Double.valueOf(d);
        } catch (NumberFormatException e) {
            return null;
        }
    }

    /** Frame size for print mode: fzf preview window, then the terminal, then COLUMNS/LINES, then 80x24. */
    static int[] defaultSize(boolean leaveRoomForPrompt) {
        int c = envInt("FZF_PREVIEW_COLUMNS"), r = envInt("FZF_PREVIEW_LINES");
        if (c > 0 && r > 0) return new int[] {c, r};
        int[] t = termSize();
        if (t != null) return new int[] {t[0], Math.max(2, leaveRoomForPrompt ? t[1] - 1 : t[1])};
        c = envInt("COLUMNS");
        r = envInt("LINES");
        if (c > 0 && r > 0) return new int[] {c, r};
        return new int[] {80, 24};
    }

    static int envInt(String name) {
        try {
            String v = System.getenv(name);
            return v == null ? 0 : Integer.parseInt(v.trim());
        } catch (RuntimeException e) {
            return 0;
        }
    }

    static byte[] readAll(InputStream in) throws IOException {
        ByteArrayOutputStream bo = new ByteArrayOutputStream();
        byte[] buf = new byte[1 << 16];
        for (int n; (n = in.read(buf)) > 0; ) bo.write(buf, 0, n);
        return bo.toByteArray();
    }

    // ========================================================================
    // STL loading
    // ========================================================================

    static void load(byte[] d) {
        float[] t = isBinary(d) ? readBinary(d) : readAscii(d);

        // drop triangles with non-finite coordinates
        int n = 0;
        for (int i = 0; i + 9 <= t.length; i += 9) {
            boolean ok = true;
            for (int k = 0; k < 9; k++) {
                float f = t[i + k];
                if (Float.isNaN(f) || Float.isInfinite(f)) ok = false;
            }
            if (ok) {
                if (n != i) System.arraycopy(t, i, t, n, 9);
                n += 9;
            }
        }
        nt = n / 9;
        if (nt == 0) throw new Fail(fileName + ": no triangles found (is this an STL file?)");

        // weld identical vertices
        int nc = nt * 3;
        int cap = 16;
        while (cap < nc * 2L && cap < (1 << 30)) cap <<= 1;
        int[] tab = new int[cap];
        Arrays.fill(tab, -1);
        float[] px = new float[nc], py = new float[nc], pz = new float[nc];
        tri = new int[nc];
        nv = 0;
        for (int c = 0; c < nc; c++) {
            float x = t[3 * c] + 0f, y = t[3 * c + 1] + 0f, z = t[3 * c + 2] + 0f;
            int h = mix(Float.floatToIntBits(x) * 73856093 ^ Float.floatToIntBits(y) * 19349663
                ^ Float.floatToIntBits(z) * 83492791);
            int i = h & (cap - 1);
            while (true) {
                int v = tab[i];
                if (v < 0) {
                    tab[i] = nv;
                    px[nv] = x; py[nv] = y; pz[nv] = z;
                    tri[c] = nv++;
                    break;
                }
                if (px[v] == x && py[v] == y && pz[v] == z) {
                    tri[c] = v;
                    break;
                }
                i = (i + 1) & (cap - 1);
            }
        }
        t = null;
        tab = null;
        vx = Arrays.copyOf(px, nv);
        vy = Arrays.copyOf(py, nv);
        vz = Arrays.copyOf(pz, nv);
        px = py = pz = null;

        // centre on the bounding box and scale to a unit sphere
        double x0 = Double.MAX_VALUE, y0 = x0, z0 = x0, x1 = -x0, y1 = -x0, z1 = -x0;
        for (int i = 0; i < nv; i++) {
            x0 = Math.min(x0, vx[i]); x1 = Math.max(x1, vx[i]);
            y0 = Math.min(y0, vy[i]); y1 = Math.max(y1, vy[i]);
            z0 = Math.min(z0, vz[i]); z1 = Math.max(z1, vz[i]);
        }
        dimX = x1 - x0; dimY = y1 - y0; dimZ = z1 - z0;
        double cx = (x0 + x1) / 2, cy = (y0 + y1) / 2, cz = (z0 + z1) / 2, r2 = 0;
        for (int i = 0; i < nv; i++) {
            double dx = vx[i] - cx, dy = vy[i] - cy, dz = vz[i] - cz;
            r2 = Math.max(r2, dx * dx + dy * dy + dz * dz);
        }
        double inv = r2 > 0 ? 1 / Math.sqrt(r2) : 1;
        for (int i = 0; i < nv; i++) {
            vx[i] = (float) ((vx[i] - cx) * inv);
            vy[i] = (float) ((vy[i] - cy) * inv);
            vz[i] = (float) ((vz[i] - cz) * inv);
        }

        // face normals (computed from the vertices; normals in the file are often junk)
        fnx = new float[nt]; fny = new float[nt]; fnz = new float[nt];
        for (int f = 0; f < nt; f++) {
            int a = tri[3 * f], b = tri[3 * f + 1], c = tri[3 * f + 2];
            double ux = vx[b] - vx[a], uy = vy[b] - vy[a], uz = vz[b] - vz[a];
            double wx = vx[c] - vx[a], wy = vy[c] - vy[a], wz = vz[c] - vz[a];
            double nx = uy * wz - uz * wy, ny = uz * wx - ux * wz, nz = ux * wy - uy * wx;
            double l = Math.sqrt(nx * nx + ny * ny + nz * nz);
            if (l > 0) {
                fnx[f] = (float) (nx / l); fny[f] = (float) (ny / l); fnz[f] = (float) (nz / l);
            }
        }

        buildEdges();
    }

    static void buildEdges() {
        int nc = nt * 3;
        int cap = 16;
        while (cap < nc * 1.5 && cap < (1 << 30)) cap <<= 1;
        long[] keys = new long[cap];
        int[] idx = new int[cap];
        Arrays.fill(keys, -1L);
        ea = new int[nc]; eb = new int[nc]; ef1 = new int[nc]; ef2 = new int[nc];
        eflag = new byte[nc];
        boolean[] messy = new boolean[nc];
        ne = 0;
        for (int f = 0; f < nt; f++) {
            for (int k = 0; k < 3; k++) {
                int a = tri[3 * f + k], b = tri[3 * f + (k + 1) % 3];
                if (a == b) continue;
                boolean fwd = a < b;
                int lo = fwd ? a : b, hi = fwd ? b : a;
                long key = ((long) lo << 32) | hi;
                int i = mix((int) (key ^ (key >>> 32)) + lo * 31) & (cap - 1);
                while (true) {
                    if (keys[i] == -1L) {
                        keys[i] = key;
                        idx[i] = ne;
                        ea[ne] = lo; eb[ne] = hi; ef1[ne] = f; ef2[ne] = -1;
                        eflag[ne] = (byte) (fwd ? 1 : 0);
                        ne++;
                        break;
                    }
                    if (keys[i] == key) {
                        int e = idx[i];
                        if (ef2[e] < 0) {
                            ef2[e] = f;
                            // a well-formed pair walks the shared edge in opposite directions
                            if (((eflag[e] & 1) != 0) == fwd) eflag[e] |= 2;
                        } else {
                            messy[e] = true;
                        }
                        break;
                    }
                    i = (i + 1) & (cap - 1);
                }
            }
        }
        double cosCrease = Math.cos(Math.toRadians(crease));
        for (int e = 0; e < ne; e++) {
            boolean feat;
            if (ef2[e] < 0 || messy[e]) {
                feat = true;
            } else {
                int f = ef1[e], g = ef2[e];
                double dot = fnx[f] * fnx[g] + fny[f] * fny[g] + fnz[f] * fnz[g];
                if ((eflag[e] & 2) != 0) dot = -dot;
                boolean degenerate = (fnx[f] == 0 && fny[f] == 0 && fnz[f] == 0)
                    || (fnx[g] == 0 && fny[g] == 0 && fnz[g] == 0);
                feat = !degenerate && dot < cosCrease;
            }
            if (feat) eflag[e] |= 4;
        }
    }

    static int mix(int h) {
        h ^= h >>> 16; h *= 0x85ebca6b;
        h ^= h >>> 13; h *= 0xc2b2ae35;
        h ^= h >>> 16;
        return h;
    }

    static boolean isBinary(byte[] d) {
        if (d.length >= 84) {
            long n = ByteBuffer.wrap(d, 80, 4).order(ByteOrder.LITTLE_ENDIAN).getInt() & 0xffffffffL;
            if (84 + 50 * n == d.length) return true;
        }
        String head = new String(d, 0, Math.min(d.length, 4096), StandardCharsets.ISO_8859_1)
            .trim().toLowerCase(Locale.ROOT);
        if (head.startsWith("solid") && (head.contains("facet") || d.length < 84)) return false;
        return true;
    }

    static float[] readBinary(byte[] d) {
        if (d.length < 84) throw new Fail(fileName + ": too short to be an STL file");
        ByteBuffer bb = ByteBuffer.wrap(d).order(ByteOrder.LITTLE_ENDIAN);
        long n = bb.getInt(80) & 0xffffffffL;
        if (n == 0 || 84 + 50 * n > d.length)
            throw new Fail(fileName + ": not an STL file (or a truncated one)");
        if (n > 40000000L) throw new Fail(fileName + ": too many triangles (" + n + ")");
        float[] t = new float[(int) n * 9];
        for (int i = 0; i < n; i++) {
            int off = 84 + 50 * i + 12;
            for (int k = 0; k < 9; k++) t[9 * i + k] = bb.getFloat(off + 4 * k);
        }
        return t;
    }

    static float[] readAscii(byte[] d) {
        float[] t = new float[9 * 1024];
        int n = 0, i = 0, len = d.length;
        while (i < len) {
            while (i < len && (d[i] & 0xff) <= ' ') i++;
            int st = i;
            while (i < len && (d[i] & 0xff) > ' ') i++;
            if (i - st == 6 && (d[st] | 32) == 'v' && (d[st + 1] | 32) == 'e' && (d[st + 2] | 32) == 'r'
                && (d[st + 3] | 32) == 't' && (d[st + 4] | 32) == 'e' && (d[st + 5] | 32) == 'x') {
                if (n + 3 > t.length) t = Arrays.copyOf(t, t.length * 2);
                for (int k = 0; k < 3; k++) {
                    while (i < len && (d[i] & 0xff) <= ' ') i++;
                    int s2 = i;
                    while (i < len && (d[i] & 0xff) > ' ') i++;
                    try {
                        t[n++] = Float.parseFloat(new String(d, s2, i - s2, StandardCharsets.ISO_8859_1));
                    } catch (NumberFormatException e) {
                        throw new Fail(fileName + ": bad vertex in ASCII STL near byte " + s2);
                    }
                }
            }
        }
        return Arrays.copyOf(t, n - n % 9);
    }

    // ========================================================================
    // Rendering
    // ========================================================================

    static float[] X = new float[0], Y, Z;       // projected vertices (sample units)
    static float[] faceNz, faceLum;
    static float[] zbuf = new float[0];
    static int[] idbuf;
    static float[] edgeZ = new float[0];
    static char[] edgeCh;
    static int[] edgeHits;                       // samples an edge left in each cell
    static int bufW, bufH, bufCols;

    /** Renders the current view into a cols x rows grid of characters. */
    static char[] render(int cols, int rows) {
        int W = cols * SX, H = rows * SY;
        double ratio = aspect * SX / SY;                 // sample height / sample width
        double base = 0.94 * Math.min(W / 2.0, H / 2.0 * ratio);
        double s = base * zoom;
        double ox = W / 2.0 + panX * base, oy = H / 2.0 - panY * base / ratio;

        // view = R * (x, z, -y): model Z is up, model -Y faces the viewer
        double m00 = R[0], m01 = -R[2], m02 = R[1];
        double m10 = R[3], m11 = -R[5], m12 = R[4];
        double m20 = R[6], m21 = -R[8], m22 = R[7];

        if (X.length < nv) { X = new float[nv]; Y = new float[nv]; Z = new float[nv]; }
        double sy = s / ratio;
        for (int i = 0; i < nv; i++) {
            double x = vx[i], y = vy[i], z = vz[i];
            X[i] = (float) (ox + (m00 * x + m01 * y + m02 * z) * s);
            Y[i] = (float) (oy - (m10 * x + m11 * y + m12 * z) * sy);
            Z[i] = (float) ((m20 * x + m21 * y + m22 * z) * s);
        }

        if (faceNz == null) { faceNz = new float[nt]; faceLum = new float[nt]; }
        final double lx = -0.45, ly = 0.65, lz = 0.61;   // light: up and to the left, in front
        for (int f = 0; f < nt; f++) {
            double x = fnx[f], y = fny[f], z = fnz[f];
            double nx = m00 * x + m01 * y + m02 * z;
            double ny = m10 * x + m11 * y + m12 * z;
            double nz = m20 * x + m21 * y + m22 * z;
            double d = nx * lx + ny * ly + nz * lz;
            if (nz < 0) d = -d;                           // two-sided: light whichever side we see
            faceNz[f] = (float) nz;
            faceLum[f] = (float) (0.12 + 0.68 * Math.max(0, d) + 0.20 * Math.abs(nz));
        }

        if (zbuf.length < W * H) { zbuf = new float[W * H]; idbuf = new int[W * H]; }
        if (edgeZ.length < cols * rows) { edgeZ = new float[cols * rows]; edgeCh = new char[cols * rows]; edgeHits = new int[cols * rows]; }
        Arrays.fill(zbuf, 0, W * H, Float.NEGATIVE_INFINITY);
        Arrays.fill(idbuf, 0, W * H, -1);
        Arrays.fill(edgeZ, 0, cols * rows, Float.NEGATIVE_INFINITY);
        Arrays.fill(edgeCh, 0, cols * rows, (char) 0);
        Arrays.fill(edgeHits, 0, cols * rows, 0);
        bufW = W; bufH = H; bufCols = cols;

        if (mode != XRAY) rasterFaces(W, H);

        if (mode != SHADE) {
            boolean all = mode == MESH || mode == XRAY;
            boolean depth = mode != XRAY;
            double slash = Math.toDegrees(Math.atan(aspect));
            for (int e = 0; e < ne; e++) {
                if (!all && (eflag[e] & 4) == 0) {
                    // not a feature edge: still draw it where it forms the silhouette
                    float a = faceNz[ef1[e]], b = faceNz[ef2[e]];
                    if ((eflag[e] & 2) != 0) b = -b;
                    if (!(a * b < 0)) continue;
                }
                drawEdge(e, depth, ratio, slash);
            }
        }

        char[] out = new char[cols * rows];
        boolean shade = mode == BOTH || mode == SHADE;
        int levels = RAMP.length();
        for (int r = 0; r < rows; r++) {
            for (int c = 0; c < cols; c++) {
                int cell = r * cols + c;
                // a single stray sample means the line only clipped this cell's corner
                char ch = edgeHits[cell] >= 2 ? edgeCh[cell] : 0;
                if (ch == 0) {
                    ch = ' ';
                    if (shade) {
                        int n = 0;
                        float sum = 0;
                        for (int j = 0; j < SY; j++) {
                            int row = (r * SY + j) * W + c * SX;
                            for (int k = 0; k < SX; k++) {
                                int id = idbuf[row + k];
                                if (id >= 0) { n++; sum += faceLum[id]; }
                            }
                        }
                        if (n > 0) {
                            double v = sum / n * (0.5 + 0.5 * n / (SX * SY));
                            int li = Math.max(0, Math.min(levels - 1, (int) (v * levels)));
                            ch = RAMP.charAt(invert ? levels - 1 - li : li);
                        }
                    }
                }
                out[cell] = ch;
            }
        }
        return out;
    }

    static void rasterFaces(int W, int H) {
        for (int f = 0; f < nt; f++) {
            int ia = tri[3 * f], ib = tri[3 * f + 1], ic = tri[3 * f + 2];
            float ax = X[ia], ay = Y[ia], bx = X[ib], by = Y[ib], cx = X[ic], cy = Y[ic];
            int x0 = Math.max(0, (int) Math.ceil(Math.min(ax, Math.min(bx, cx)) - 0.5f));
            int x1 = Math.min(W - 1, (int) Math.floor(Math.max(ax, Math.max(bx, cx)) - 0.5f));
            if (x0 > x1) continue;
            int y0 = Math.max(0, (int) Math.ceil(Math.min(ay, Math.min(by, cy)) - 0.5f));
            int y1 = Math.min(H - 1, (int) Math.floor(Math.max(ay, Math.max(by, cy)) - 0.5f));
            if (y0 > y1) continue;
            double area = (double) (bx - ax) * (cy - ay) - (double) (by - ay) * (cx - ax);
            if (area == 0) continue;
            double inv = 1 / area;
            float az = Z[ia], bz = Z[ib], cz = Z[ic];
            for (int py = y0; py <= y1; py++) {
                double sy = py + 0.5;
                int row = py * W;
                for (int px = x0; px <= x1; px++) {
                    double sx = px + 0.5;
                    double w0 = ((bx - sx) * (cy - sy) - (by - sy) * (cx - sx)) * inv;
                    double w1 = ((cx - sx) * (ay - sy) - (cy - sy) * (ax - sx)) * inv;
                    double w2 = 1 - w0 - w1;
                    if (w0 < -1e-6 || w1 < -1e-6 || w2 < -1e-6) continue;
                    float z = (float) (w0 * az + w1 * bz + w2 * cz);
                    if (z > zbuf[row + px]) {
                        zbuf[row + px] = z;
                        idbuf[row + px] = f;
                    }
                }
            }
        }
    }

    static void drawEdge(int e, boolean depth, double ratio, double slash) {
        int a = ea[e], b = eb[e];
        double x0 = X[a], y0 = Y[a], z0 = Z[a];
        double dx = X[b] - x0, dy = Y[b] - y0, dz = Z[b] - z0;

        // clip the segment to the frame (Liang-Barsky)
        double t0 = 0, t1 = 1;
        double[] p = {-dx, dx, -dy, dy};
        double[] q = {x0, bufW - 1e-3 - x0, y0, bufH - 1e-3 - y0};
        for (int i = 0; i < 4; i++) {
            if (p[i] == 0) {
                if (q[i] < 0) return;
            } else {
                double r = q[i] / p[i];
                if (p[i] < 0) { if (r > t1) return; if (r > t0) t0 = r; }
                else { if (r < t0) return; if (r < t1) t1 = r; }
            }
        }

        // pick the glyph closest to the on-screen direction of the line
        double ang = Math.toDegrees(Math.atan2(Math.abs(dy * ratio), Math.abs(dx)));
        char ch;
        if (ang < slash / 2) ch = '-';
        else if (ang > (slash + 90) / 2) ch = '|';
        else ch = (dx * dy > 0) ? '\\' : '/';

        // two samples per sample-width, so a cell the line really crosses collects at least two hits
        int steps = (int) Math.ceil(Math.max(Math.abs(dx), Math.abs(dy)) * (t1 - t0) * 2) + 1;
        for (int i = 0; i < steps; i++) {
            double t = t0 + (t1 - t0) * i / steps;
            int px = (int) (x0 + dx * t), py = (int) (y0 + dy * t);
            if (px < 0 || py < 0 || px >= bufW || py >= bufH) continue;
            float z = (float) (z0 + dz * t);
            if (depth && !visible(px, py, z)) continue;
            int cell = (py / SY) * bufCols + px / SX;
            edgeHits[cell]++;
            if (z > edgeZ[cell]) {
                edgeZ[cell] = z;
                edgeCh[cell] = ch;
            }
        }
    }

    /**
     * An edge sample is hidden only if every sample around it holds a nearer surface.
     * Looking at the 3x3 neighbourhood keeps edges on steep faces and silhouettes
     * from being eaten by their own surface.
     */
    static boolean visible(int px, int py, float z) {
        float lim = z + 0.5f;
        int xa = Math.max(0, px - 1), xb = Math.min(bufW - 1, px + 1);
        int ya = Math.max(0, py - 1), yb = Math.min(bufH - 1, py + 1);
        for (int y = ya; y <= yb; y++) {
            int row = y * bufW;
            for (int x = xa; x <= xb; x++) if (zbuf[row + x] <= lim) return true;
        }
        return false;
    }

    /** The frame as plain text: one line per row, trailing blanks removed. */
    static String frameText(int cols, int rows) {
        char[] g = render(cols, rows);
        StringBuilder sb = new StringBuilder(cols * rows + rows);
        for (int r = 0; r < rows; r++) {
            int end = cols;
            while (end > 0 && g[r * cols + end - 1] == ' ') end--;
            sb.append(g, r * cols, end).append('\n');
        }
        return sb.toString();
    }

    // ========================================================================
    // Rotation helpers
    // ========================================================================

    static double[] axisX(double deg) {
        double c = Math.cos(Math.toRadians(deg)), s = Math.sin(Math.toRadians(deg));
        return new double[] {1, 0, 0, 0, c, -s, 0, s, c};
    }

    static double[] axisY(double deg) {
        double c = Math.cos(Math.toRadians(deg)), s = Math.sin(Math.toRadians(deg));
        return new double[] {c, 0, s, 0, 1, 0, -s, 0, c};
    }

    static double[] axisZ(double deg) {
        double c = Math.cos(Math.toRadians(deg)), s = Math.sin(Math.toRadians(deg));
        return new double[] {c, -s, 0, s, c, 0, 0, 0, 1};
    }

    static double[] mul(double[] a, double[] b) {
        double[] m = new double[9];
        for (int i = 0; i < 3; i++)
            for (int j = 0; j < 3; j++)
                m[3 * i + j] = a[3 * i] * b[j] + a[3 * i + 1] * b[3 + j] + a[3 * i + 2] * b[6 + j];
        return m;
    }

    /** R = Rx * Ry * Rz. */
    static double[] euler(double rx, double ry, double rz) {
        return mul(axisX(rx), mul(axisY(ry), axisZ(rz)));
    }

    /** Folds an angle into (-180, 180]. */
    static double wrap(double d) {
        d = Math.IEEEremainder(d, 360);
        return d <= -180 ? d + 360 : d;
    }

    static String num(double d, int places) {
        String s = String.format(Locale.ROOT, "%." + places + "f", d);
        if (s.indexOf('.') >= 0) s = s.replaceAll("0+$", "").replaceAll("\\.$", "");
        return s.equals("-0") ? "0" : s;
    }

    /** The arguments that reproduce the current view in print mode. */
    static String viewArgs() {
        String s = num(rotX, 1) + " " + num(rotY, 1) + " " + num(rotZ, 1) + " " + num(zoom, 3);
        if (panX != 0 || panY != 0) s += " " + num(panX, 3) + " " + num(panY, 3);
        return s;
    }

    // ========================================================================
    // Terminal
    // ========================================================================

    /** Runs stty against the controlling terminal; null if that is not possible. */
    static String stty(String... args) {
        try {
            List<String> cmd = new ArrayList<String>();
            cmd.add("stty");
            cmd.addAll(Arrays.asList(args));
            ProcessBuilder pb = new ProcessBuilder(cmd);
            pb.redirectInput(new File("/dev/tty"));
            pb.redirectErrorStream(true);
            Process p = pb.start();
            String out = new String(readAll(p.getInputStream()), StandardCharsets.ISO_8859_1);
            return p.waitFor() == 0 ? out.trim() : null;
        } catch (Exception e) {
            return null;
        }
    }

    /** {cols, rows} of the controlling terminal, or null. */
    static int[] termSize() {
        String s = stty("size");
        if (s == null) return null;
        try {
            String[] p = s.split("\\s+");
            int rows = Integer.parseInt(p[0]), cols = Integer.parseInt(p[1]);
            return rows > 0 && cols > 0 ? new int[] {cols, rows} : null;
        } catch (RuntimeException e) {
            return null;
        }
    }

    static boolean stdoutIsTty() {
        try {
            Process p = new ProcessBuilder("sh", "-c", "test -t 1")
                .redirectOutput(ProcessBuilder.Redirect.INHERIT).start();
            return p.waitFor() == 0;
        } catch (Exception e) {
            return System.console() != null;
        }
    }

    // ========================================================================
    // Interactive mode
    // ========================================================================

    static final OutputStream TTY_OUT = new FileOutputStream(FileDescriptor.out);

    static void put(String s) {
        try {
            TTY_OUT.write(s.getBytes(StandardCharsets.US_ASCII));
            TTY_OUT.flush();
        } catch (IOException e) {
            // the terminal went away; nothing useful to do
        }
    }

    static void interactive() throws IOException {
        final String saved = stty("-g");
        int[] sz = termSize();
        if (saved == null || sz == null)
            throw new Fail("interactive mode needs a terminal and stty; use -p to print a frame instead");
        Runtime.getRuntime().addShutdownHook(new Thread() {
            @Override public void run() {
                put("\033[0m\033[?25h\033[?1049l");
                stty(saved);
            }
        });
        // keys arrive one at a time, unechoed; reads give up after 0.5s so resizes get noticed
        stty("-icanon", "-echo", "min", "0", "time", "5");
        put("\033[?1049h\033[?25l");

        InputStream in = new FileInputStream("/dev/tty");
        byte[] buf = new byte[256];
        String msg = null;
        boolean help = false, dirty = true;
        while (true) {
            if (dirty) {
                draw(sz[0], sz[1], help ? HELP_LINE : msg);
                dirty = false;
            }
            int n = in.read(buf);
            if (n <= 0) {
                int[] now = termSize();
                if (now == null) System.exit(0);
                if (now[0] != sz[0] || now[1] != sz[1]) {
                    sz = now;
                    dirty = true;
                }
                continue;
            }
            msg = null;
            for (int i = 0; i < n; i++) {
                int k = buf[i] & 0xff;
                if (k == 27) {
                    if (i + 2 < n && (buf[i + 1] == '[' || buf[i + 1] == 'O')) {
                        int arrow = "ABCD".indexOf(buf[i + 2]);   // up, down, right, left
                        k = arrow < 0 ? 0 : "wsda".charAt(arrow);
                        i += 2;
                    } else if (n == 1) {
                        System.exit(0);
                    } else {
                        continue;
                    }
                }
                switch (k) {
                    case 'w': rotX -= ROT_STEP; break;
                    case 's': rotX += ROT_STEP; break;
                    case 'a': rotY -= ROT_STEP; break;
                    case 'd': rotY += ROT_STEP; break;
                    case 'z': rotZ += ROT_STEP; break;
                    case 'x': rotZ -= ROT_STEP; break;
                    case 'W': panY += PAN_STEP; break;
                    case 'S': panY -= PAN_STEP; break;
                    case 'A': panX -= PAN_STEP; break;
                    case 'D': panX += PAN_STEP; break;
                    case 'i': case '+': case '=': zoom = Math.min(500, zoom * ZOOM_STEP); break;
                    case 'o': case '-': zoom = Math.max(0.02, zoom / ZOOM_STEP); break;
                    case 'm': mode = (mode + 1) % MODES.length; break;
                    case 'r': case '0':
                        rotX = DEF_RX; rotY = DEF_RY; rotZ = DEF_RZ;
                        R = euler(rotX, rotY, rotZ);
                        zoom = 1; panX = panY = 0;
                        break;
                    case 'p': R = euler(rotX, rotY, rotZ); msg = save(frameText(sz[0], Math.max(1, sz[1] - 1))); break;
                    case '?': case 'h': help = !help; break;
                    case 'q': case 3: case 4: System.exit(0); break;
                    default: break;
                }
            }
            rotX = wrap(rotX); rotY = wrap(rotY); rotZ = wrap(rotZ);
            R = euler(rotX, rotY, rotZ);
            dirty = true;
        }
    }

    static final String HELP_LINE =
        " ws tilt | ad spin | zx roll | WASD pan | i/o zoom | m mode | r reset | p save | ? help | q quit";

    static void draw(int cols, int rows, String msg) {
        int vr = Math.max(1, rows - 1);
        char[] g = render(cols, vr);
        StringBuilder sb = new StringBuilder(cols * rows + rows * 8 + 64);
        sb.append("\033[H");
        for (int r = 0; r < vr; r++) {
            int end = cols;
            while (end > 0 && g[r * cols + end - 1] == ' ') end--;
            sb.append(g, r * cols, end).append("\033[K\r\n");
        }
        String status = msg != null ? msg
            : " " + fileName + " | " + nt + " tris | " + num(dimX, 1) + " x " + num(dimY, 1) + " x " + num(dimZ, 1)
              + " | " + MODES[mode] + " | view " + viewArgs() + " | ? help";
        StringBuilder st = new StringBuilder();
        for (int i = 0; i < status.length() && st.length() < cols - 1; i++) {
            char ch = status.charAt(i);
            st.append(ch >= ' ' && ch < 127 ? ch : '?');
        }
        while (st.length() < cols - 1) st.append(' ');
        sb.append("\033[7m").append(st).append("\033[0m\033[K");
        put(sb.toString());
    }

    /** Writes the frame next to where you are (or to the temp dir if that is read-only). */
    static String save(String text) {
        String base = fileName.replaceFirst("(?i)\\.stl$", "");
        String[] dirs = {System.getProperty("user.dir"), System.getProperty("java.io.tmpdir")};
        String err = "";
        for (String dir : dirs) {
            for (int n = 1; n < 1000; n++) {
                File f = new File(dir, base + ".rndr" + (n == 1 ? "" : "-" + n) + ".txt");
                if (f.exists()) continue;
                try (OutputStream os = new FileOutputStream(f)) {
                    os.write(text.getBytes(StandardCharsets.US_ASCII));
                    return " saved " + f.getPath() + "   (rndr " + fileName + " " + viewArgs() + ")";
                } catch (IOException e) {
                    err = e.getMessage();
                    break;
                }
            }
        }
        return " save failed: " + err;
    }
}
