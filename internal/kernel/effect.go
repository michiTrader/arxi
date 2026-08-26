package kernel

// Effect es algo que el reducer quiere que pase en el mundo real. El reducer no
// lo hace: lo describe. El ejecutor lo cumple y devuelve el resultado como un
// evento nuevo.
//
// La interfaz tiene un método no exportado (isEffect) para que sea sellada:
// ningún paquete fuera de kernel puede agregar variantes. Es la forma que tiene
// Go de aproximarse a un enum de Rust, y la razón de que la aproximación sea
// aceptable está en ADR-0007: lo que Go no da en el compilador lo damos con
// TestEffectExhaustivo, que falla si alguien agrega una variante y se olvida de
// registrarla.
type Effect interface {
	isEffect()
	Class() EffectClass
}

// EffectClass resuelve el hueco que marcó IA A: si Decide devuelve
// []Effect, ¿el ejecutor los corre en orden o en paralelo?
//
// La respuesta no puede ser "todos en paralelo" porque Emit y SetTimer cambian
// lo que el resto del sistema va a ver, ni "todos en orden" porque entonces
// tres turnos de agente que no se conocen entre sí se serializan y perdemos la
// razón de ser de la herramienta.
//
// Así que se parten en dos clases: control primero y en orden de lista,
// independientes después y en paralelo. El reducer ordena la lista antes de
// devolverla (ver orderEffects), así que el ejecutor solo necesita esta regla:
// correr el prefijo de control secuencialmente, luego el resto concurrente.
type EffectClass int

const (
	// ClassControl cambia el estado observable del run o el reloj. Se ejecutan
	// en el orden exacto de la lista, uno después de otro.
	ClassControl EffectClass = iota
	// ClassIndependent son efectos que solo se afectan a sí mismos. Se pueden
	// ejecutar en paralelo entre ellos.
	ClassIndependent
)

// SpawnTurn abre un turno de agente. Es el efecto caro: cada uno es una llamada
// a un proveedor con dinero real de por medio.
//
// Coalesced dice cuántas causas se fusionaron en este turno. Si tres eventos
// despertaron al mismo agente mientras estaba ocupado, se abre UN turno con las
// tres causas en el contexto, no tres turnos. Ese número aparece en el evento
// para que se pueda auditar cuánto ahorró el coalescing.
type SpawnTurn struct {
	Agent       string
	Context     ContextSpec
	CauseEvents []string
	BudgetSlice float64
	Coalesced   int
}

func (SpawnTurn) isEffect()          {}
func (SpawnTurn) Class() EffectClass { return ClassIndependent }

// CallTool ejecuta una herramienta. Idempotent viene de la declaración de la
// superficie, no de una heurística: si el ejecutor se cae después de mandar la
// llamada y antes de escribir el resultado, solo puede reintentar sin preguntar
// las que están declaradas idempotentes.
type CallTool struct {
	Agent      string
	Tool       string
	Args       map[string]any
	Idempotent bool
}

func (CallTool) isEffect()          {}
func (CallTool) Class() EffectClass { return ClassIndependent }

// Emit pide escribir un evento derivado en el log. Es control porque el evento
// que escribe cambia el estado que van a ver los efectos siguientes.
//
// El Seq del evento va en 0: el reducer no asigna números de secuencia porque
// no es el escritor único del log. El ejecutor pone el seq al escribir.
type Emit struct{ Event Event }

func (Emit) isEffect()          {}
func (Emit) Class() EffectClass { return ClassControl }

// SetTimer arma un timer. FiresAtMs es un offset relativo en milisegundos, no
// un timestamp absoluto, precisamente para que el reloj virtual de --sim pueda
// ejecutar el mismo fold sin esperar media hora de verdad.
type SetTimer struct {
	ID        string
	FiresAtMs int64
}

func (SetTimer) isEffect()          {}
func (SetTimer) Class() EffectClass { return ClassControl }

// CancelTimer desarma un timer. Es control: si se ejecutara en paralelo con el
// Emit que lo hace innecesario, habría una carrera entre "la etapa avanzó" y
// "la etapa expiró" y el run terminaría de dos formas distintas según la suerte.
type CancelTimer struct{ ID string }

func (CancelTimer) isEffect()          {}
func (CancelTimer) Class() EffectClass { return ClassControl }

// AskHuman crea un item de inbox y espera. OnTimeout es lo que hay que hacer si
// nadie contesta: es obligatorio decidirlo acá y no en el momento del timeout,
// porque en el momento del timeout ya no hay nadie mirando.
type AskHuman struct {
	Kind      string
	Question  string
	Agent     string
	OnTimeout string
	TimeoutMs int64
}

func (AskHuman) isEffect()          {}
func (AskHuman) Class() EffectClass { return ClassIndependent }

// Snapshot materializa el estado en el seq dado para que `run show` no tenga
// que reproducir el log entero. Es control porque debe verse consistente con
// los eventos ya emitidos en este mismo paso.
type Snapshot struct{ AtSeq int64 }

func (Snapshot) isEffect()          {}
func (Snapshot) Class() EffectClass { return ClassControl }

// allEffectVariants es el registro que hace posible el test de exhaustividad.
// Si agregás una variante de Effect y no la agregás acá, TestEffectExhaustivo
// falla. Es el reemplazo mecánico del `match` exhaustivo que da Rust gratis.
var allEffectVariants = []Effect{
	SpawnTurn{},
	CallTool{},
	Emit{},
	SetTimer{},
	CancelTimer{},
	AskHuman{},
	Snapshot{},
}

// EffectVariants devuelve una copia del registro de variantes. Copia y no el
// slice directo para que un test no pueda corromper el registro de otro.
func EffectVariants() []Effect {
	out := make([]Effect, len(allEffectVariants))
	copy(out, allEffectVariants)
	return out
}
