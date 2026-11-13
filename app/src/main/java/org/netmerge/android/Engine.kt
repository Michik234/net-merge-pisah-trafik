package org.netmerge.android

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * App-wide state shared by the VPN service, the UI and the Quick Settings tile. The service
 * samples the Go engine once a second; the UI only observes.
 */
object Engine {
    enum class Phase { OFF, STARTING, ON }

    /** Seconds of throughput history kept for the live chart. */
    const val HISTORY = 60

    private val _phase = MutableStateFlow(Phase.OFF)
    val phase: StateFlow<Phase> = _phase.asStateFlow()

    private val _stats = MutableStateFlow<List<Stats>>(emptyList())
    val stats: StateFlow<List<Stats>> = _stats.asStateFlow()

    /** Per network: total (down + up) bytes/s for the last [HISTORY] seconds, oldest first. */
    private val _history = MutableStateFlow<Map<String, List<Float>>>(emptyMap())
    val history: StateFlow<Map<String, List<Float>>> = _history.asStateFlow()

    private val _evenSplit = MutableStateFlow(false)
    val evenSplit: StateFlow<Boolean> = _evenSplit.asStateFlow()

    /**
     * Split mode: connections to the game ports use mobile data, everything else uses Wi-Fi, at the
     * same time. Ports are lists such as "7000-8000,3074".
     */
    data class SplitSettings(
        val enabled: Boolean = false,
        val udp: String = "",
        val tcp: String = "",
        val strict: Boolean = false,
    )

    private val _split = MutableStateFlow(SplitSettings())
    val split: StateFlow<SplitSettings> = _split.asStateFlow()

    val running: Boolean get() = _phase.value == Phase.ON

    internal fun setPhase(p: Phase) {
        _phase.value = p
        if (p == Phase.OFF) {
            _stats.value = emptyList()
            _history.value = emptyMap()
        }
    }

    /** Reads the engine's counters. Called by the service every second while it runs. */
    internal fun sample() {
        val now = Stats.parse(netmerge.Netmerge.stats()).filter { it.present }
        _stats.value = now
        val old = _history.value
        _history.value = now.associate { s ->
            val prev = old[s.name] ?: List(HISTORY) { 0f }
            s.name to (prev + (s.rateIn + s.rateOut).toFloat()).takeLast(HISTORY)
        }
    }

    // --- settings ---

    private fun prefs(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE)

    fun loadSettings(ctx: Context) {
        _evenSplit.value = prefs(ctx).getBoolean("even_split", false)
        val p = prefs(ctx)
        _split.value = SplitSettings(
            enabled = p.getBoolean("split_enabled", false),
            udp = p.getString("split_udp", "") ?: "",
            tcp = p.getString("split_tcp", "") ?: "",
            strict = p.getBoolean("split_strict", false),
        )
        applySettings()
    }

    fun setEvenSplit(ctx: Context, on: Boolean) {
        prefs(ctx).edit().putBoolean("even_split", on).apply()
        _evenSplit.value = on
        applySettings()
    }

    /**
     * Applies and saves split mode. Returns null on success, or a message for the user when the
     * ports are not valid (nothing is changed then).
     */
    fun setSplit(ctx: Context, s: SplitSettings): String? {
        val clean = s.copy(udp = s.udp.trim(), tcp = s.tcp.trim())
        val none = { p: String -> p.isEmpty() || p.equals("none", ignoreCase = true) }
        if (clean.enabled && none(clean.udp) && none(clean.tcp)) {
            return "Enter at least one game port first, for example 7000-8000,3074"
        }
        val err = runCatching {
            netmerge.Netmerge.setSplit(clean.enabled, clean.udp, clean.tcp, clean.strict)
        }.exceptionOrNull()
        if (err != null) return err.message ?: "Invalid ports"
        prefs(ctx).edit()
            .putBoolean("split_enabled", clean.enabled)
            .putString("split_udp", clean.udp)
            .putString("split_tcp", clean.tcp)
            .putBoolean("split_strict", clean.strict)
            .apply()
        _split.value = clean
        return null
    }

    private fun applySettings() {
        runCatching { netmerge.Netmerge.setAutoWeights(!_evenSplit.value) }
        val s = _split.value
        runCatching { netmerge.Netmerge.setSplit(s.enabled, s.udp, s.tcp, s.strict) }
    }
}
