<template>
  <section class="sequence">
    <header class="columns page-header">
      <div class="column is-8">
        <p class="is-size-7 mb-2">
          <router-link :to="{ name: 'sequences' }">
            &larr; Sequences
          </router-link>
        </p>
        <h1 class="title is-4 mb-2">
          {{ seq.name }}
          <b-tag v-if="seq.id" :class="isOn ? 'running' : 'paused'">
            {{ isOn ? 'On' : 'Off' }}
          </b-tag>
        </h1>
        <p v-if="seq.id" class="is-size-7 has-text-grey">
          <template v-if="isOn">
            Anyone who joins <strong>{{ seq.listName }}</strong> from now on gets these e-mails.
          </template>
          <template v-else-if="seq.activatedAt">
            Paused. Nobody is enrolled or e-mailed until you switch it back on.
          </template>
          <template v-else>
            Not started. Switching it on never e-mails the people already on the list.
          </template>
        </p>
      </div>
      <div class="column has-text-right">
        <b-button v-if="seq.id && $can('campaigns:send')" :type="isOn ? 'is-light' : 'is-primary'"
          :icon-left="isOn ? 'pause-circle-outline' : 'rocket-launch-outline'" :loading="loading.campaigns"
          @click="toggleStatus" data-cy="btn-sequence-status">
          {{ isOn ? 'Switch off' : 'Switch on' }}
        </b-button>
      </div>
    </header>

    <b-notification v-if="seq.lastError" type="is-danger is-light" :closable="false" role="alert">
      <strong>This sequence hit a problem and some e-mails are waiting.</strong>
      {{ seq.lastError }}. It tries again every minute, so nothing is lost once this is fixed.
    </b-notification>

    <b-tabs v-if="seq.id" type="is-boxed" :animated="false" v-model="tab">
      <b-tab-item label="E-mails" label-position="on-border" value="steps" icon="email-outline">
        <section class="wrap">
          <p v-if="steps.length === 0" class="has-text-grey mb-4">
            No e-mails yet. Add the first one below.
          </p>

          <div v-for="(st, n) in steps" :key="st.id" class="box mb-3" :data-cy="`step-${n + 1}`">
            <div class="columns is-vcentered is-mobile is-multiline">
              <div class="column is-narrow">
                <span class="tag is-light is-medium">{{ n + 1 }}</span>
              </div>

              <div class="column is-12-mobile is-4-tablet">
                <b-field grouped :label="n === 0 ? 'Send after joining' : 'Send after the previous e-mail'"
                  custom-class="is-small">
                  <b-numberinput v-model="st.delayValue" :min="0" :max="999" :controls="false" size="is-small"
                    :disabled="!canManage" @input="markDirty(st)" style="width: 5rem" />
                  <b-select v-model="st.delayUnit" size="is-small" :disabled="!canManage" @input="markDirty(st)">
                    <option value="minutes">
                      minutes
                    </option>
                    <option value="hours">
                      hours
                    </option>
                    <option value="days">
                      days
                    </option>
                  </b-select>
                  <b-button v-if="st.dirty" size="is-small" type="is-primary" @click="saveStep(st)">
                    {{ $t('globals.buttons.save') }}
                  </b-button>
                </b-field>
                <p v-if="st.delayValue === 0" class="is-size-7 has-text-grey">
                  Sends straight away (within a minute).
                </p>
              </div>

              <div class="column">
                <router-link :to="{ name: 'campaign', params: { id: st.campaignId }, hash: '#content' }">
                  <strong>{{ st.campaignSubject || st.campaignName }}</strong>
                </router-link>
                <p class="is-size-7 has-text-grey">
                  {{ st.campaignName }}
                </p>
                <p class="is-size-7">
                  Sent {{ $utils.formatNumber(st.numSent) }}
                  &middot; Opens {{ $utils.formatNumber(st.numViews) }}
                  &middot; Clicks {{ $utils.formatNumber(st.numClicks) }}
                </p>
              </div>

              <div v-if="canManage" class="column is-narrow actions">
                <a href="#" v-if="n > 0" @click.prevent="moveStep(n, -1)" aria-label="Move up">
                  <b-tooltip label="Move up" type="is-dark">
                    <b-icon icon="arrow-up" size="is-small" />
                  </b-tooltip>
                </a>
                <a href="#" v-if="n < steps.length - 1" @click.prevent="moveStep(n, 1)" aria-label="Move down">
                  <b-tooltip label="Move down" type="is-dark">
                    <b-icon icon="arrow-down" size="is-small" />
                  </b-tooltip>
                </a>
                <a href="#" @click.prevent="deleteStep(st)" aria-label="Remove">
                  <b-tooltip label="Remove from sequence" type="is-dark">
                    <b-icon icon="trash-can-outline" size="is-small" />
                  </b-tooltip>
                </a>
              </div>
            </div>
          </div>

          <div v-if="canManage" class="box has-background-white-bis">
            <h5 class="title is-6 mb-4">
              Add e-mail {{ steps.length + 1 }}
            </h5>
            <b-field grouped group-multiline
              :label="steps.length === 0 ? 'Send after joining' : 'Send after the previous e-mail'">
              <b-numberinput v-model="newStep.delayValue" :min="0" :max="999" :controls="false"
                style="width: 6rem" data-cy="new-step-delay" />
              <b-select v-model="newStep.delayUnit" data-cy="new-step-unit">
                <option value="minutes">
                  minutes
                </option>
                <option value="hours">
                  hours
                </option>
                <option value="days">
                  days
                </option>
              </b-select>
            </b-field>

            <div class="buttons mt-4">
              <b-button type="is-primary" icon-left="plus" :loading="loading.campaigns" @click="writeNewEmail"
                data-cy="btn-new-step-email">
                Write a new e-mail
              </b-button>
              <b-button v-if="!isPicking" @click="showPicker" data-cy="btn-pick-step-email">
                Use an existing draft
              </b-button>
            </div>

            <div v-if="isPicking">
              <b-field label="Draft campaign"
                :message="`Drafts addressed to ${seq.listName} are shown.`">
                <b-select v-model="newStep.campaignId" expanded placeholder="Choose a draft"
                  data-cy="new-step-campaign">
                  <option v-for="c in drafts" :value="c.id" :key="c.id">
                    {{ c.name }}
                  </option>
                </b-select>
              </b-field>
              <p v-if="drafts.length === 0" class="is-size-7 has-text-grey mb-3">
                No drafts are addressed to this list yet.
              </p>
              <b-button type="is-primary" :disabled="!newStep.campaignId" @click="addExistingStep"
                data-cy="btn-add-step">
                Add to sequence
              </b-button>
            </div>
          </div>
        </section>
      </b-tab-item>

      <b-tab-item :label="`People (${$utils.formatNumber(numPeople)})`" label-position="on-border" value="people"
        icon="account-multiple">
        <section class="wrap">
          <b-field grouped>
            <b-select v-model="people.status" @input="onPeopleFilter" size="is-small">
              <option value="">
                Everyone
              </option>
              <option value="active">
                In progress ({{ seq.numActive }})
              </option>
              <option value="completed">
                Finished ({{ seq.numCompleted }})
              </option>
              <option value="exited">
                Left early ({{ seq.numExited }})
              </option>
            </b-select>
          </b-field>

          <b-table :data="people.results" :loading="people.loading" hoverable paginated backend-pagination
            :current-page="people.page" :per-page="people.perPage" :total="people.total"
            @page-change="onPeoplePage">
            <b-table-column v-slot="props" field="email" :label="$t('subscribers.email')">
              <router-link :to="{ name: 'subscriber', params: { id: props.row.subscriberId } }">
                {{ props.row.email }}
              </router-link>
              <p class="is-size-7 has-text-grey">
                {{ props.row.name }}
              </p>
            </b-table-column>

            <b-table-column v-slot="props" field="status" :label="$t('globals.fields.status')">
              <b-tag :class="peopleTagClass(props.row.status)">
                {{ peopleStatusLabel(props.row) }}
              </b-tag>
            </b-table-column>

            <b-table-column v-slot="props" field="lastPosition" label="E-mails received" numeric>
              {{ props.row.lastPosition }} of {{ steps.length }}
            </b-table-column>

            <b-table-column v-slot="props" field="enrolledAt" label="Joined">
              {{ $utils.niceDate(props.row.enrolledAt, true) }}
            </b-table-column>

            <b-table-column v-slot="props" field="nextDueAt" label="Next e-mail">
              <template v-if="props.row.status === 'active' && props.row.nextDueAt">
                {{ isOn ? $utils.niceDate(props.row.nextDueAt, true) : 'Paused' }}
              </template>
              <template v-else>
                &mdash;
              </template>
            </b-table-column>

            <b-table-column v-slot="props" cell-class="actions" align="right">
              <a v-if="canManage && props.row.status === 'active'" href="#" @click.prevent="removePerson(props.row)"
                aria-label="Remove from sequence">
                <b-tooltip label="Remove from sequence" type="is-dark">
                  <b-icon icon="cancel" size="is-small" />
                </b-tooltip>
              </a>
            </b-table-column>

            <template #empty v-if="!people.loading">
              <p class="has-text-grey has-text-centered p-5">
                Nobody yet. People appear here as they join {{ seq.listName }}.
              </p>
            </template>
          </b-table>
        </section>
      </b-tab-item>

      <b-tab-item :label="$t('globals.terms.settings')" label-position="on-border" value="settings"
        icon="cog-outline">
        <section class="wrap">
          <form @submit.prevent="saveSettings" class="columns">
            <div class="column is-7">
              <b-field :label="$t('globals.fields.name')" label-position="on-border">
                <b-input :maxlength="200" v-model="form.name" name="name" required :disabled="!canManage" />
              </b-field>

              <b-field label="Starts when someone joins" label-position="on-border"
                :message="steps.length > 0 ? 'The list can\'t be changed once the sequence has e-mails.' : ''">
                <b-select v-model="form.listId" name="list" required expanded
                  :disabled="!canManage || steps.length > 0">
                  <option v-for="l in allLists" :value="l.id" :key="l.id">
                    {{ l.name }}
                  </option>
                </b-select>
              </b-field>

              <list-selector v-model="form.exitLists" :selected="form.exitLists" :all="exitChoices"
                :disabled="!canManage" label="Stop the sequence for anyone on these lists"
                placeholder="Choose lists, for example your client list"
                message="Unsubscribing or being blocklisted always stops it. Use this to also stop it when someone becomes a client, books in, and so on." />

              <b-field v-if="canManage" class="mt-5">
                <b-button native-type="submit" type="is-primary" icon-left="content-save-outline"
                  :loading="loading.campaigns" data-cy="btn-save-sequence">
                  {{ $t('globals.buttons.save') }}
                </b-button>
              </b-field>
            </div>
          </form>
        </section>
      </b-tab-item>
    </b-tabs>
  </section>
</template>

<script>
import Vue from 'vue';
import { mapState } from 'vuex';
import ListSelector from '../components/ListSelector.vue';

const UNIT_MINUTES = { minutes: 1, hours: 60, days: 1440 };

// Split a wait in minutes into the largest whole unit, e.g. 4320 => 3 days.
const fromMinutes = (mins) => {
  if (mins > 0 && mins % 1440 === 0) {
    return { delayValue: mins / 1440, delayUnit: 'days' };
  }
  if (mins > 0 && mins % 60 === 0) {
    return { delayValue: mins / 60, delayUnit: 'hours' };
  }
  if (mins === 0) {
    return { delayValue: 0, delayUnit: 'days' };
  }
  return { delayValue: mins, delayUnit: 'minutes' };
};

const toMinutes = (value, unit) => Math.max(0, parseInt(value, 10) || 0) * UNIT_MINUTES[unit];

export default Vue.extend({
  components: {
    ListSelector,
  },

  data() {
    return {
      tab: 'steps',
      seq: {},
      steps: [],
      form: { name: '', listId: null, exitLists: [] },
      newStep: { delayValue: 1, delayUnit: 'days', campaignId: null },
      isPicking: false,
      drafts: [],
      people: {
        results: [], total: 0, page: 1, perPage: 20, status: '', loading: false,
      },
    };
  },

  methods: {
    setSequence(data) {
      this.seq = data;
      this.steps = (data.steps || []).map((s) => ({ ...s, ...fromMinutes(s.delayMinutes), dirty: false }));
      this.form = {
        name: data.name,
        listId: data.listId,
        exitLists: this.allLists.filter((l) => (data.exitListIds || []).indexOf(l.id) > -1),
      };

      // The first e-mail usually goes straight away, later ones a day or more apart.
      if (this.steps.length === 0) {
        this.newStep.delayValue = 0;
      } else if (this.newStep.delayValue === 0) {
        this.newStep.delayValue = 1;
      }
    },

    getSequence() {
      return this.$api.getSequence(this.id).then((data) => {
        this.setSequence(data);
      });
    },

    toggleStatus() {
      const fn = () => {
        this.$api.updateSequenceStatus(this.id, this.isOn ? 'paused' : 'active').then((data) => {
          this.setSequence(data);
          this.$utils.toast(this.isOn ? 'Sequence switched on' : 'Sequence switched off');
        });
      };

      if (this.isOn) {
        fn();
        return;
      }

      const msg = this.seq.activatedAt
        ? 'Switch the sequence back on? Anyone waiting on an e-mail that came due while it was off gets it now.'
        : `Switch the sequence on? Everyone who joins "${this.seq.listName}" from now on gets these e-mails. People already on the list are not e-mailed.`;
      this.$utils.confirm(msg, fn);
    },

    saveSettings() {
      this.$api.updateSequence(this.id, {
        name: this.form.name,
        list_id: this.form.listId,
        exit_list_ids: this.form.exitLists.map((l) => l.id),
      }).then((data) => {
        this.setSequence(data);
        this.$utils.toast(this.$t('globals.messages.updated', { name: data.name }));
      });
    },

    markDirty(st) {
      this.$set(st, 'dirty', true);
    },

    saveStep(st) {
      this.$api.updateSequenceStep(this.id, st.id, {
        delay_minutes: toMinutes(st.delayValue, st.delayUnit),
      }).then((data) => {
        this.setSequence(data);
        this.$utils.toast('Timing saved');
      });
    },

    moveStep(n, dir) {
      const ids = this.steps.map((s) => s.id);
      const [id] = ids.splice(n, 1);
      ids.splice(n + dir, 0, id);

      this.$api.orderSequenceSteps(this.id, ids).then((data) => {
        this.setSequence(data);
      });
    },

    deleteStep(st) {
      this.$utils.confirm(
        `Remove "${st.campaignName}" from the sequence? The e-mail stays as a draft campaign.`,
        () => {
          this.$api.deleteSequenceStep(this.id, st.id).then((data) => {
            this.setSequence(data);
          });
        },
      );
    },

    // Create a draft campaign addressed to the sequence's list, add it as
    // the next step and open it in the normal campaign editor.
    writeNewEmail() {
      const n = this.steps.length + 1;
      const name = `${this.seq.name}: e-mail ${n}`;

      this.$api.createCampaign({
        name,
        subject: name,
        lists: [this.seq.listId],
        from_email: this.serverConfig.from_email,
        content_type: 'richtext',
        messenger: 'email',
        type: 'regular',
        tags: ['sequence'],
      }).then((camp) => this.$api.createSequenceStep(this.id, {
        campaign_id: camp.id,
        delay_minutes: toMinutes(this.newStep.delayValue, this.newStep.delayUnit),
      }).then(() => {
        this.$utils.toast('E-mail added. Write it here, then come back to the sequence.', 'is-success', 6000);
        this.$router.push({ name: 'campaign', hash: '#content', params: { id: camp.id } });
      }));
    },

    showPicker() {
      this.isPicking = true;
      const used = this.steps.map((s) => s.campaignId);

      this.$api.getCampaigns({ status: 'draft', per_page: 'all' }).then((data) => {
        this.drafts = (data.results || []).filter((c) => c.type === 'regular'
          && used.indexOf(c.id) === -1
          && (c.lists || []).some((l) => l.id === this.seq.listId));
      });
    },

    addExistingStep() {
      this.$api.createSequenceStep(this.id, {
        campaign_id: this.newStep.campaignId,
        delay_minutes: toMinutes(this.newStep.delayValue, this.newStep.delayUnit),
      }).then((data) => {
        this.isPicking = false;
        this.newStep.campaignId = null;
        this.setSequence(data);
      });
    },

    getPeople() {
      this.people.loading = true;
      this.$api.getSequenceSubscribers(this.id, {
        page: this.people.page,
        per_page: this.people.perPage,
        status: this.people.status,
      }).then((data) => {
        this.people.results = data.results || [];
        this.people.total = data.total;
      }).finally(() => {
        this.people.loading = false;
      });
    },

    onPeoplePage(p) {
      this.people.page = p;
      this.getPeople();
    },

    onPeopleFilter() {
      this.people.page = 1;
      this.getPeople();
    },

    removePerson(row) {
      this.$utils.confirm(
        `Remove ${row.email} from this sequence? They get no more of its e-mails. They stay on the list.`,
        () => {
          this.$api.removeSequenceSubscriber(this.id, row.subscriberId).then(() => {
            this.refresh();
          });
        },
      );
    },

    peopleTagClass(status) {
      if (status === 'active') {
        return 'running';
      }
      return status === 'completed' ? 'finished' : 'cancelled';
    },

    peopleStatusLabel(row) {
      if (row.status === 'active') {
        return 'In progress';
      }
      if (row.status === 'completed') {
        return 'Finished';
      }
      return row.exitReason ? `Left: ${row.exitReason}` : 'Left early';
    },

    refresh() {
      this.getSequence();
      this.getPeople();
    },
  },

  computed: {
    ...mapState(['loading', 'lists', 'serverConfig']),

    id() {
      return parseInt(this.$route.params.id, 10);
    },

    isOn() {
      return this.seq.status === 'active';
    },

    canManage() {
      return this.$can('campaigns:manage');
    },

    allLists() {
      return this.lists.results || [];
    },

    // Any list other than the one that starts the sequence.
    exitChoices() {
      return this.allLists.filter((l) => l.id !== this.form.listId);
    },

    numPeople() {
      return (this.seq.numActive || 0) + (this.seq.numCompleted || 0) + (this.seq.numExited || 0);
    },
  },

  created() {
    this.$root.$on('page.refresh', this.refresh);
  },

  destroyed() {
    this.$root.$off('page.refresh', this.refresh);
  },

  mounted() {
    // Lists first, so the exit lists can be shown by name.
    this.$api.getLists({ minimal: true, per_page: 'all', status: 'active' }).then(() => {
      this.getSequence();
      this.getPeople();
    });
  },
});
</script>
