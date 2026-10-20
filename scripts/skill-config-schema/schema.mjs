// scripts/skill-config-schema/schema.mjs — the JSON Schema for .boss-skills.json.
//
// The single source of the HARD (throwing) constraints skills-toolbox/skill-config.mjs
// validateConfig enforces. build.mjs compiles it with ajv into a dependency-free standalone module
// (skills-toolbox/skill-config-validate.generated.mjs) that the vendored toolboxes import. Soft
// constraints — a value that warns and falls back to a default — stay in validateConfig, so a
// value with a soft rule is declared here as a known key with no constraint.
//
// Two annotation keywords carry what JSON Schema cannot:
//   - `x-msg`: the operator-facing message for a failure under this node. A string covers every
//     failure at this node and below it (unless a deeper node has its own); an object maps an ajv
//     keyword to a message for failures of that keyword directly on this node. A missing required
//     property is reported as a `type` failure of that property's own schema. Placeholders:
//     `{N}` instance path segment N, `{json}` the JSON of the value this node validates, `{.k}`
//     that value's property k, `{extra}` the unknown key or property name.
//   - `x-unknown: 'warn'`: an unknown key in this closed object warns and is ignored instead of
//     failing. Without it, `additionalProperties: false` fails like any other constraint.

export const VERIFY_PATH_LABEL_MAX = 60
export const ADAPTER_KINDS = ['tracker', 'publish', 'sessionRunner']
export const PLAN_SECTION_REQUIRED_KINDS = ['always', 'needs-human', 'open-questions', 'optional']
export const VERIFY_HUMAN_REVIEW_CATEGORIES = ['database', 'security', 'ui']

const nonEmptyString = { type: 'string', minLength: 1 }
// A string with at least one non-whitespace character (JS `s.trim() !== ''`).
const nonBlankString = { type: 'string', pattern: '\\S' }
// One line with at least one non-whitespace character. The leading class excludes \S so the
// pattern stays linear; overlapping `[^\r\n]*\S` backtracks quadratically on long strings.
const singleLine = { type: 'string', pattern: '^[^\\S\\r\\n]*\\S[^\\r\\n]*$' }
const object = (extra = {}) => ({ type: 'object', ...extra })
const known = {}

const GLOB_LIST_SHAPE = `an array of non-empty glob strings or {glob, label} objects (label: one line, at most ${VERIFY_PATH_LABEL_MAX} chars)`
const globList = (msg) => ({
  type: 'array',
  'x-msg': msg,
  items: {
    anyOf: [
      nonEmptyString,
      object({
        required: ['glob'],
        properties: {
          glob: nonEmptyString,
          label: { ...singleLine, maxLength: VERIFY_PATH_LABEL_MAX },
        },
        additionalProperties: false,
      }),
    ],
  },
})

const safeInteger = (minimum) => ({
  type: 'integer',
  minimum,
  maximum: Number.MAX_SAFE_INTEGER,
  'x-msg': 'retro.{1} must be a valid integer',
})

const retro = object({
  'x-msg': { type: 'retro must be an object', additionalProperties: 'retro.{extra} is unknown' },
  properties: {
    maxIssues: safeInteger(0),
    staleDays: safeInteger(0),
    minRuns: safeInteger(1),
    gateThreshold: safeInteger(1),
    pathAliases: object({
      'x-msg': 'retro.pathAliases must map non-empty strings',
      propertyNames: nonBlankString,
      additionalProperties: nonBlankString,
    }),
    guidanceAudit: object({ 'x-msg': 'retro.guidanceAudit must be an object' }),
  },
  additionalProperties: false,
})

const verifyDefaults = object({
  'x-msg': { type: 'verifyDefaults must be an object when present' },
  // Open on purpose: an older tree ignores a knob a newer one added.
  properties: {
    alwaysHumanPaths: globList(`verifyDefaults.alwaysHumanPaths must be ${GLOB_LIST_SHAPE}`),
    requireGreenBase: {
      type: 'boolean',
      'x-msg': 'verifyDefaults.requireGreenBase must be a boolean when present',
    },
    generatedPaths: globList(`verifyDefaults.generatedPaths must be ${GLOB_LIST_SHAPE}`),
    generatedChecks: {
      type: 'array',
      items: singleLine,
      'x-msg':
        'verifyDefaults.generatedChecks must be an array of non-empty, single-line check names',
    },
    humanReview: object({
      'x-msg': {
        type: 'verifyDefaults.humanReview must be an object when present',
        additionalProperties: `verifyDefaults.humanReview.{extra} is unknown (expected ${VERIFY_HUMAN_REVIEW_CATEGORIES.join(', ')})`,
      },
      properties: Object.fromEntries(
        VERIFY_HUMAN_REVIEW_CATEGORIES.map((category) => [
          category,
          globList(`verifyDefaults.humanReview.${category} must be ${GLOB_LIST_SHAPE}`),
        ]),
      ),
      additionalProperties: false,
    }),
  },
})

const roleMap = (field) =>
  object({
    'x-msg': { type: `trackerConfig.{1}.${field} must be an object when present` },
    additionalProperties: {
      ...nonEmptyString,
      'x-msg': `trackerConfig.{1}.${field}.{3} must be a non-empty string`,
    },
  })

const trackerAdapter = object({
  'x-msg': { type: 'trackerConfig.{1} must be an object' },
  required: ['mcpServer'],
  // Open: adapter-specific identity keys a newer core reads must not fail an older copy.
  properties: {
    mcpServer: {
      ...nonEmptyString,
      'x-msg': 'trackerConfig.{1}.mcpServer must be a non-empty string',
    },
    ...Object.fromEntries(
      ['team', 'teamKey', 'workspace'].map((field) => [
        field,
        {
          ...nonEmptyString,
          'x-msg': `trackerConfig.{1}.${field} must be a non-empty string when present`,
        },
      ]),
    ),
    states: roleMap('states'),
    labels: roleMap('labels'),
    githubLabels: roleMap('githubLabels'),
    descriptionNormalization: object({
      'x-msg': {
        type: 'trackerConfig.{1}.descriptionNormalization must be an object when present',
      },
      properties: {
        tolerated: {
          type: 'array',
          'x-msg':
            'trackerConfig.{1}.descriptionNormalization.tolerated must be an array of transform ids',
          items: {
            ...nonEmptyString,
            'x-msg':
              'trackerConfig.{1}.descriptionNormalization.tolerated entries must be non-empty strings; got {json}',
          },
        },
        onUnattributedDrift: {
          ...nonEmptyString,
          'x-msg':
            'trackerConfig.{1}.descriptionNormalization.onUnattributedDrift must be a non-empty string; got {json}',
        },
      },
    }),
    followUpLabels: {
      type: 'array',
      minItems: 1,
      items: nonEmptyString,
      'x-msg': 'trackerConfig.{1}.followUpLabels must be a non-empty array of non-empty strings',
    },
    // Owned by skills-toolbox/selection.mjs validateSelectionBlock, which validateConfig still calls.
    selection: known,
  },
})

const publishAdapter = object({
  'x-msg': { type: 'publishConfig.{1} must be an object' },
  required: ['bucket', 'baseUrl'],
  properties: {
    bucket: { ...nonEmptyString, 'x-msg': 'publishConfig.{1}.bucket must be a non-empty string' },
    baseUrl: { ...nonEmptyString, 'x-msg': 'publishConfig.{1}.baseUrl must be a non-empty string' },
  },
})

const planDependencyList = (key) => ({
  type: 'array',
  'x-msg': `planDependencies.${key} must be an array`,
  items: {
    ...nonEmptyString,
    'x-msg': `planDependencies.${key} entries must be non-empty strings`,
  },
})

const AREA_ALIAS_MSG =
  'planDependencies.areaAliases.{extra} must map to a non-empty string or an array of them'

const planDependencies = object({
  'x-msg': {
    type: 'planDependencies must be an object',
    additionalProperties: 'planDependencies.{extra} is not a known key',
  },
  properties: {
    moduleRoots: planDependencyList('moduleRoots'),
    repoWideTokens: planDependencyList('repoWideTokens'),
    areaAliases: object({
      'x-msg': { type: 'planDependencies.areaAliases must be an object' },
      propertyNames: { minLength: 1, 'x-msg': AREA_ALIAS_MSG },
      additionalProperties: {
        'x-msg':
          'planDependencies.areaAliases.{2} must map to a non-empty string or an array of them',
        anyOf: [nonEmptyString, { type: 'array', items: nonEmptyString }],
      },
    }),
  },
  additionalProperties: false,
})

const planContract = object({
  'x-msg': { type: 'planContract must be an object' },
  required: ['version', 'sections'],
  properties: {
    version: {
      type: 'integer',
      minimum: 1,
      'x-msg': 'planContract.version must be a positive integer',
    },
    sections: {
      type: 'array',
      minItems: 1,
      'x-msg': {
        type: 'planContract.sections must be a non-empty array',
        minItems: 'planContract.sections must be a non-empty array',
      },
      items: object({
        'x-msg': { type: 'planContract.sections entries must be objects' },
        allOf: [
          {
            'x-msg': 'planContract.sections entry {json} needs a non-empty string heading',
            required: ['heading'],
            properties: { heading: nonEmptyString },
          },
          {
            'x-msg': `planContract.sections entry "{.heading}" required must be one of ${PLAN_SECTION_REQUIRED_KINDS.join(', ')}`,
            required: ['required'],
            properties: { required: { enum: PLAN_SECTION_REQUIRED_KINDS } },
          },
        ],
      }),
    },
  },
  additionalProperties: false,
  'x-unknown': 'warn',
})

/** Top-level keys in the order validateConfig reports them; the first failing key wins. */
export const schema = object({
  required: [
    'env',
    'adapters',
    'extensionRoots',
    'trackerConfig',
    'publishConfig',
    'planStorage',
    'planContract',
  ],
  properties: {
    retro,
    // Retired review machinery: accepted here, then warned about and dropped by validateConfig.
    lensMap: known,
    reviewLedger: known,
    // Retired merge-consent block (docs/skills/skill-config.md): still accepted, silently ignored.
    completionDefaults: known,
    reviewDefaults: object({
      'x-msg': { type: 'reviewDefaults must be an object when present' },
      properties: {
        softBudgetMinutes: known,
        hangGuardMinutes: known,
        rounds: known,
        deltaFileThreshold: known,
        maxDispatchedRounds: known,
        forceFull: known,
      },
      additionalProperties: false,
      'x-unknown': 'warn',
    }),
    // A malformed `review` warns and falls back to the default review, so it has no `type`; the
    // key list still applies whenever it is an object.
    review: {
      properties: { secondModel: known, skill: known },
      additionalProperties: false,
      'x-unknown': 'warn',
    },
    epicDefaults: object({
      'x-msg': { type: 'epicDefaults must be an object when present' },
      properties: { childWallClockMinutes: known },
      additionalProperties: false,
      'x-unknown': 'warn',
    }),
    notesDefaults: object({
      'x-msg': { type: 'notesDefaults must be an object when present' },
      properties: { sampleRate: known },
      additionalProperties: false,
      'x-unknown': 'warn',
    }),
    verifyDefaults,
    commands: object({
      'x-msg': { type: 'commands must be an object when present' },
      additionalProperties: {
        ...nonEmptyString,
        'x-msg': 'commands.{1} must be a non-empty string',
      },
    }),
    test: object({
      'x-msg': { type: 'test must be an object when present' },
      properties: {
        manifestPath: {
          ...nonEmptyString,
          'x-msg': 'test.manifestPath must be a non-empty string when present',
        },
      },
      additionalProperties: false,
      'x-unknown': 'warn',
    }),
    env: object({
      'x-msg': { type: 'env.headlessSignals must be an array' },
      required: ['headlessSignals', 'headlessWhenNoTty'],
      properties: {
        headlessSignals: {
          type: 'array',
          'x-msg': { type: 'env.headlessSignals must be an array' },
          items: object({
            'x-msg': { type: 'env.headlessSignals entries must be objects' },
            allOf: [
              {
                'x-msg': 'env.headlessSignals entry {json} needs a non-empty string var',
                required: ['var'],
                properties: { var: nonEmptyString },
              },
              {
                'x-msg': 'env.headlessSignals entry "{.var}" needs present:true or a string equals',
                anyOf: [
                  { required: ['present'], properties: { present: { const: true } } },
                  { required: ['equals'], properties: { equals: { type: 'string' } } },
                ],
              },
            ],
          }),
        },
        headlessWhenNoTty: { type: 'boolean', 'x-msg': 'env.headlessWhenNoTty must be a boolean' },
      },
    }),
    adapters: object({
      'x-msg': { type: 'adapters must be an object' },
      required: ADAPTER_KINDS,
      properties: Object.fromEntries(
        ADAPTER_KINDS.map((kind) => [
          kind,
          { ...nonEmptyString, 'x-msg': `adapters.${kind} must be a non-empty string` },
        ]),
      ),
    }),
    extensionRoots: {
      type: 'array',
      minItems: 1,
      items: nonEmptyString,
      'x-msg': 'extensionRoots must be a non-empty array of non-empty strings',
    },
    trackerConfig: object({
      'x-msg': { type: 'trackerConfig must be an object' },
      additionalProperties: trackerAdapter,
    }),
    publishConfig: object({
      'x-msg': { type: 'publishConfig must be an object' },
      additionalProperties: publishAdapter,
    }),
    planStorage: object({
      'x-msg': { type: 'planStorage must be an object' },
      required: ['kind'],
      properties: {
        // "r2" is deprecated: validateConfig warns and rewrites it.
        kind: {
          enum: ['tracker-attachment', 'r2'],
          'x-msg': 'planStorage.kind must be "tracker-attachment"',
        },
      },
      additionalProperties: false,
      'x-unknown': 'warn',
    }),
    planDependencies,
    planContract,
    // Read by other toolbox modules, not validated here.
    gateCache: known,
    builtinExtensions: known,
  },
  additionalProperties: false,
  'x-unknown': 'warn',
})
