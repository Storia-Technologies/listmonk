<template>
  <section class="sequences">
    <header class="columns page-header">
      <div class="column is-10">
        <h1 class="title is-4 mb-2">
          Sequences
          <span v-if="sequences.length > 0">({{ sequences.length }})</span>
        </h1>
        <p class="is-size-7 has-text-grey">
          A sequence sends a series of e-mails to each person who joins a list, timed from when they joined.
        </p>
      </div>
      <div class="column has-text-right">
        <b-field v-if="$can('campaigns:manage')" expanded>
          <b-button expanded type="is-primary" icon-left="plus" class="btn-new" @click="showNewForm"
            data-cy="btn-new-sequence">
            {{ $t('globals.buttons.new') }}
          </b-button>
        </b-field>
      </div>
    </header>

    <b-table :data="sequences" :loading="loading.campaigns" hoverable>
      <b-table-column v-slot="props" field="name" :label="$t('globals.fields.name')" width="25%">
        <router-link :to="{ name: 'sequence', params: { id: props.row.id } }">
          {{ props.row.name }}
        </router-link>
        <p v-if="props.row.lastError" class="is-size-7 has-text-danger">
          Needs attention
        </p>
      </b-table-column>

      <b-table-column v-slot="props" field="status" :label="$t('globals.fields.status')" width="10%">
        <b-tag :class="props.row.status === 'active' ? 'running' : 'paused'">
          {{ props.row.status === 'active' ? 'On' : 'Off' }}
        </b-tag>
      </b-table-column>

      <b-table-column v-slot="props" field="listName" label="Starts when someone joins">
        {{ props.row.listName }}
      </b-table-column>

      <b-table-column v-slot="props" field="numSteps" label="E-mails" numeric>
        {{ props.row.numSteps }}
      </b-table-column>

      <b-table-column v-slot="props" field="numActive" label="In progress" numeric>
        {{ $utils.formatNumber(props.row.numActive) }}
      </b-table-column>

      <b-table-column v-slot="props" field="numCompleted" label="Finished" numeric>
        {{ $utils.formatNumber(props.row.numCompleted) }}
      </b-table-column>

      <b-table-column v-slot="props" field="numSent" label="Sent" numeric>
        {{ $utils.formatNumber(props.row.numSent) }}
      </b-table-column>

      <b-table-column v-slot="props" cell-class="actions" align="right">
        <div>
          <router-link :to="{ name: 'sequence', params: { id: props.row.id } }" data-cy="btn-edit-sequence"
            :aria-label="$t('globals.buttons.edit')">
            <b-tooltip :label="$t('globals.buttons.edit')" type="is-dark">
              <b-icon icon="pencil-outline" size="is-small" />
            </b-tooltip>
          </router-link>
          <a v-if="$can('campaigns:manage')" href="#" @click.prevent="deleteSequence(props.row)"
            data-cy="btn-delete-sequence" :aria-label="$t('globals.buttons.delete')">
            <b-tooltip :label="$t('globals.buttons.delete')" type="is-dark">
              <b-icon icon="trash-can-outline" size="is-small" />
            </b-tooltip>
          </a>
        </div>
      </b-table-column>

      <template #empty v-if="!loading.campaigns">
        <empty-placeholder />
      </template>
    </b-table>

    <b-modal scroll="keep" :aria-modal="true" :active.sync="isFormVisible" :width="600">
      <form @submit.prevent="createSequence">
        <div class="modal-card content" style="width: auto">
          <header class="modal-card-head">
            <h4>New sequence</h4>
          </header>
          <section class="modal-card-body">
            <b-field :label="$t('globals.fields.name')" label-position="on-border">
              <b-input :maxlength="200" v-model="form.name" name="name" ref="focus" required autofocus
                placeholder="Welcome series" />
            </b-field>

            <b-field label="Starts when someone joins" label-position="on-border"
              message="Only people who join this list after you switch the sequence on get it. Nobody already on the list is e-mailed.">
              <b-select v-model="form.listId" name="list" required expanded placeholder="Choose a list">
                <option v-for="l in allLists" :value="l.id" :key="l.id">
                  {{ l.name }}
                </option>
              </b-select>
            </b-field>
          </section>
          <footer class="modal-card-foot has-text-right">
            <b-button @click="isFormVisible = false">
              {{ $t('globals.buttons.close') }}
            </b-button>
            <b-button native-type="submit" type="is-primary" :loading="loading.campaigns"
              data-cy="btn-save-sequence">
              {{ $t('globals.buttons.continue') }}
            </b-button>
          </footer>
        </div>
      </form>
    </b-modal>
  </section>
</template>

<script>
import Vue from 'vue';
import { mapState } from 'vuex';
import EmptyPlaceholder from '../components/EmptyPlaceholder.vue';

export default Vue.extend({
  components: {
    EmptyPlaceholder,
  },

  data() {
    return {
      sequences: [],
      isFormVisible: false,
      form: { name: '', listId: null },
    };
  },

  methods: {
    getSequences() {
      this.$api.getSequences().then((data) => {
        this.sequences = data;
      });
    },

    showNewForm() {
      this.form = { name: '', listId: null };
      this.isFormVisible = true;
    },

    createSequence() {
      this.$api.createSequence({ name: this.form.name, list_id: this.form.listId, exit_list_ids: [] }).then((d) => {
        this.isFormVisible = false;
        this.$router.push({ name: 'sequence', params: { id: d.id } });
      });
    },

    deleteSequence(seq) {
      this.$utils.confirm(
        `Delete the sequence "${seq.name}"? Its e-mails stay as draft campaigns. Its history is removed.`,
        () => {
          this.$api.deleteSequence(seq.id).then(() => {
            this.getSequences();
            this.$utils.toast(this.$t('globals.messages.deleted', { name: seq.name }));
          });
        },
      );
    },
  },

  computed: {
    ...mapState(['loading', 'lists']),

    allLists() {
      return this.lists.results || [];
    },
  },

  created() {
    this.$root.$on('page.refresh', this.getSequences);
  },

  destroyed() {
    this.$root.$off('page.refresh', this.getSequences);
  },

  mounted() {
    this.getSequences();
    this.$api.getLists({ minimal: true, per_page: 'all', status: 'active' });
  },
});
</script>
